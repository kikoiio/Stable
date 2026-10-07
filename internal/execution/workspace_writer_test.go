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
