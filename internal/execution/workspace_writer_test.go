package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/workspace"
)

func writerExecutorFixture(t *testing.T) (*workspace.LifecycleService, workspace.WriterLease, agent.RunExecutor, *executorTestSandbox) {
	t.Helper()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.Limits{}, workspace.ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close(context.Background()) })
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: "0123456789abcdef0123456789abcdef"}
	scope := workspace.Scope{ProjectID: "project", SessionID: work.SessionID, Work: work, Authority: permission.Authority{RunID: "lead", SessionID: work.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "candidate"), Mode: permission.ModeBypass}}
	created, err := manager.Create(context.Background(), scope, "writer")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireWriter(context.Background(), scope, created.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	fake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte(`{"output":"controlled"}`)}}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	factory := WorkspaceWriterExecutorFactory(NewToolExecutorFactory(ToolExecutorDeps{Gate: &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}, Sandbox: fake, HelperPath: helper}), lease, manager)
	bounds, _ := json.Marshal(lease.Authority)
	executor, err := factory.ForRun(agent.ExecutionRequest{RunID: lease.RunID, Work: work, PermissionBounds: bounds})
	if err != nil {
		t.Fatal(err)
	}
	return manager, lease, executor, fake
}

func TestWorkspaceWriterUsesTrustedLeaseAndCurrentCheckout(t *testing.T) {
	manager, lease, executor, fake := writerExecutorFixture(t)
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"new.txt","content":"controlled"}`)})
	if err != nil || outcome.IsError || !fake.profile.WorkspaceIsolation || fake.profile.CandidateRoot != lease.Paths.Checkout {
		t.Fatalf("writer outcome=%+v error=%v profile=%+v", outcome, err, fake.profile)
	}
	outcome, err = executor.Execute(context.Background(), llm.ToolUse{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"base.txt"}`)})
	if err != nil || outcome.IsError || fake.profile.ProjectRoot != lease.Paths.Checkout {
		t.Fatalf("read does not use checkout: %+v %v %+v", outcome, err, fake.profile)
	}
	for _, path := range []string{".git", ".stable/private", ".mewcode/config", "../formal/new.txt"} {
		args, _ := json.Marshal(map[string]any{"file_path": path, "content": "bad"})
		outcome, err = executor.Execute(context.Background(), llm.ToolUse{ID: "attack", Name: "write_file", Arguments: args})
		if err != nil || !outcome.IsError {
			t.Fatalf("protected path %s allowed: %+v %v", path, outcome, err)
		}
	}
	if _, err := manager.ReleaseCompletedWriter(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	outcome, err = executor.Execute(context.Background(), llm.ToolUse{ID: "stale", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"base.txt"}`)})
	if err != nil || !outcome.IsError {
		t.Fatalf("stale lease allowed: %+v %v", outcome, err)
	}
}

func TestWorkspaceWriterCommandUsesBoundedLeaseProfile(t *testing.T) {
	manager, lease, executor, fake := writerExecutorFixture(t)
	defer func() {
		if _, err := manager.ReleaseCompletedWriter(context.Background(), lease); err != nil {
			t.Errorf("release workspace writer: %v", err)
		}
	}()

	_, err := executor.(*toolRunExecutor).executeCommand(context.Background(), map[string]any{"command": "printf controlled"})
	if err != nil || len(fake.argv) == 0 {
		t.Fatalf("workspace command err=%v argv=%v", err, fake.argv)
	}
	profile := fake.profile
	if !profile.WorkspaceIsolation || profile.ProjectRoot != lease.Paths.Baseline || profile.CandidateRoot != lease.Paths.Checkout || profile.RunRoot != lease.Paths.Run || profile.WorkspaceVolumeRoot != lease.Paths.Root {
		t.Fatalf("workspace command escaped trusted lease roots: profile=%+v lease=%+v", profile, lease)
	}
	if len(profile.NetworkGrants) != 0 {
		t.Fatalf("workspace command received network grants: %+v", profile.NetworkGrants)
	}
	if profile.WorkspaceProcess == nil || profile.WorkspaceProcess.WorkspaceID != lease.WorkspaceID || profile.WorkspaceProcess.RunID != lease.RunID || profile.WorkspaceProcess.Generation != lease.Generation || len(profile.WorkspaceProcess.Token) != 64 || profile.WorkspaceProcess.PID != 0 || profile.WorkspaceProcess.ProcessGroup != 0 || profile.WorkspaceProcess.StartTimeTicks != 0 {
		t.Fatalf("workspace command lacks a fresh tracked process identity: %+v", profile.WorkspaceProcess)
	}
	if profile.OnProcessStart == nil || profile.OnProcessExit == nil {
		t.Fatal("workspace command lacks durable process start/exit callbacks")
	}
}

func TestWorkspaceWriterPermissionDenyPrecedesSandbox(t *testing.T) {
	manager, lease, _, fake := writerExecutorFixture(t)

	formalSentinel := filepath.Join(lease.Scope.Authority.FormalRoot, "base.txt")
	formalBefore, err := os.ReadFile(formalSentinel)
	if err != nil {
		t.Fatal(err)
	}

	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("plan mode is rejected when creating the writer executor", func(t *testing.T) {
		planAuthority := lease.Authority
		planAuthority.Mode = permission.ModePlan
		bounds, err := json.Marshal(planAuthority)
		if err != nil {
			t.Fatal(err)
		}
		factory := WorkspaceWriterExecutorFactory(
			NewToolExecutorFactory(ToolExecutorDeps{
				Gate:       &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}},
				Sandbox:    fake,
				HelperPath: helper,
			}),
			lease,
			manager,
		)
		_, err = factory.ForRun(agent.ExecutionRequest{
			RunID:            lease.RunID,
			Work:             lease.Scope.Work,
			PermissionBounds: bounds,
		})
		if err == nil {
			t.Fatal("workspace writer factory accepted plan-mode authority")
		}
		if len(fake.argv) != 0 {
			t.Fatalf("plan-mode factory rejection reached sandbox: argv=%v", fake.argv)
		}
	})

	t.Run("permission denial stops before sandbox or mutation", func(t *testing.T) {
		gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "fixture denied"}}
		factory := WorkspaceWriterExecutorFactory(
			NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: fake, HelperPath: helper}),
			lease,
			manager,
		)
		bounds, err := json.Marshal(lease.Authority)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := factory.ForRun(agent.ExecutionRequest{
			RunID:            lease.RunID,
			Work:             lease.Scope.Work,
			PermissionBounds: bounds,
		})
		if err != nil {
			t.Fatal(err)
		}

		const target = "denied-write.txt"
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{
			ID: "denied-write", Name: "write_file",
			Arguments: json.RawMessage(`{"file_path":"denied-write.txt","content":"must not be written"}`),
		})
		if err != nil || !outcome.IsError || outcome.Status != agent.ToolDenied {
			t.Fatalf("denied writer outcome=%+v err=%v", outcome, err)
		}
		if gate.calls != 1 || gate.seen.Kind != permission.OpWrite {
			t.Fatalf("permission gate calls=%d operation=%+v, want one write authorization", gate.calls, gate.seen)
		}
		if len(fake.argv) != 0 {
			t.Fatalf("denied write reached sandbox: argv=%v", fake.argv)
		}
		if _, err := os.Lstat(filepath.Join(lease.Paths.Checkout, target)); !os.IsNotExist(err) {
			t.Fatalf("denied write mutated workspace checkout: %v", err)
		}
		formalAfter, err := os.ReadFile(formalSentinel)
		if err != nil || string(formalAfter) != string(formalBefore) {
			t.Fatalf("denied write mutated formal project: before=%q after=%q err=%v", formalBefore, formalAfter, err)
		}
	})
}

func TestWorkspaceCommandWithoutBoundedVolumeFailsClosed(t *testing.T) {
	_, lease, executor, fake := writerExecutorFixture(t)
	if err := sandbox.BoundedWorkspaceVolume(lease.Paths.Root, lease.Paths.Checkout, lease.Paths.Run); err == nil {
		t.Skip("test host happens to provide a small disk volume")
	}
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "command", Name: "command", Arguments: json.RawMessage(`{"command":"touch formal-bypass"}`)})
	if err != nil || !outcome.IsError || fake.profile.CandidateRoot != "" {
		t.Fatalf("unbounded command reached sandbox: %+v %v %+v", outcome, err, fake.profile)
	}
}

func TestWorkspaceEditBudgetChecksFullResult(t *testing.T) {
	_, lease, executor, _ := writerExecutorFixture(t)
	if err := os.WriteFile(filepath.Join(lease.Paths.Checkout, "base.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := executor.(*toolRunExecutor).estimateWorkspaceWrite(map[string]any{"old_string": "old", "new_string": string(make([]byte, workspace.DefaultLimits().MaxFileBytes+1))}, "edit_file", "base.txt")
	if !errors.Is(err, workspace.ErrQuota) {
		t.Fatalf("oversized resulting edit accepted: %v", err)
	}
}
