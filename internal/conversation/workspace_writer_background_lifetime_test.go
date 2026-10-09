package conversation

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

func TestWorkspaceBackgroundWriterSurvivesSocketDisconnectAndParentCompletion(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		if err := os.WriteFile(filepath.Join(input.ProjectRoot, "child-output.txt"), []byte("workspace-only"), 0600); err != nil {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
		}
		invocation := agentTaskTestInvocation{ctx: ctx, input: input, release: release}
		select {
		case entered <- invocation:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
		select {
		case <-invocation.release:
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "workspace write complete"}
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
	})
	role := "---\nname: builder\ndescription: isolated writer\nisolation: worktree\n---\nWrite only the assigned checkout.\n"
	svc, root, session := newAgentTaskTestService(t, runner, role)
	formal := filepath.Join(root, "formal.txt")
	const formalBytes = "formal baseline"
	if err := os.WriteFile(formal, []byte(formalBytes), 0600); err != nil {
		t.Fatal(err)
	}
	workspaceState, err := os.MkdirTemp(filepath.Dir(root), "workspace-writer-lifetime-state-")
	if err != nil {
		t.Fatal(err)
	}
	svc.deps.WorkspaceStateRoot = workspaceState
	t.Cleanup(func() { _ = os.RemoveAll(workspaceState) })

	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parent := agentTaskTestParent(t, svc, session, runID)
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent.ExecutorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: workspaceCancelAllowGate{}, Sandbox: sandbox.New(), HelperPath: helper,
	})
	svc.mu.Lock()
	svc.activeRuns[parent.RunID] = session
	svc.activeRequests[parent.RunID] = agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
	svc.mu.Unlock()

	requestCtx, disconnect := context.WithCancel(context.Background())
	background, err := svc.deps.AgentTasks.Run(requestCtx, parent, agent.AgentTaskRequest{
		AgentName: "builder", Instruction: "write in the isolated workspace", Isolation: "worktree", Background: true,
	})
	if err != nil {
		disconnect()
		t.Fatal(err)
	}
	disconnect() // Model the requesting socket closing after accepted background work returns.
	invocation := receiveAgentTaskInvocation(t, entered)
	if invocation.ctx.Err() != nil {
		t.Fatalf("request disconnect canceled background writer: %v", invocation.ctx.Err())
	}
	if invocation.input.ProjectRoot == root {
		t.Fatal("writer child was given the formal project root")
	}
	if got, err := os.ReadFile(filepath.Join(invocation.input.ProjectRoot, "child-output.txt")); err != nil || string(got) != "workspace-only" {
		t.Fatalf("child workspace write=%q err=%v", got, err)
	}

	projectRoot, scope, err := svc.workspaceScope(context.Background(), ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Get(context.Background(), scope, background.WorkspaceID)
	if err != nil || active.ID != background.WorkspaceID || active.State != workspace.StateWriting || active.WriterRunID != background.RunID || active.Generation != background.WorkspaceGeneration {
		t.Fatalf("workspace lease before parent completion = %+v err=%v, want original writing lease %+v/%d", active, err, background.WorkspaceID, background.WorkspaceGeneration)
	}

	if _, err := sessionlog.Append(root, session, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-complete-background-writer", RunID: parent.RunID, SessionID: session,
		RunSeq: 1, At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := invocation.ctx.Err(); err != nil {
		t.Fatalf("normal parent completion canceled accepted background writer: %v", err)
	}
	parentAgain, err := manager.Get(context.Background(), scope, background.WorkspaceID)
	if err != nil || parentAgain.ID != active.ID || parentAgain.State != workspace.StateWriting || parentAgain.WriterRunID != active.WriterRunID || parentAgain.Generation != active.Generation {
		t.Fatalf("parent completion changed active writer lease: %+v err=%v, before=%+v", parentAgain, err, active)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed before child exit: %q err=%v", got, err)
	}

	releaseOnce.Do(func() { close(release) })
	terminal := waitAgentTaskTerminal(t, svc, parent, background.ID)
	settled, err := manager.Get(context.Background(), scope, background.WorkspaceID)
	if err != nil || terminal.Status != agent.DelegationSucceeded || settled.ID != active.ID || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != active.Generation {
		t.Fatalf("writer did not settle after real runner exit: task=%+v workspace=%+v err=%v", terminal, settled, err)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed after child exit: %q err=%v", got, err)
	}
	svc.mu.Lock()
	delete(svc.activeRuns, parent.RunID)
	delete(svc.activeRequests, parent.RunID)
	svc.mu.Unlock()
}
