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
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

type workspaceCancelAllowGate struct{}

func (workspaceCancelAllowGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

func TestWorkspaceWriterCancellationRetainsLeaseUntilRunnerExit(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	runner := agentTaskTestRunner(func(ctx context.Context, _ agent.ChildRunInput) agent.ChildRunResult {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		<-release
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	})
	role := "---\nname: builder\ndescription: isolated writer\nisolation: worktree\n---\nWrite only the assigned checkout.\n"
	svc, root, session := newAgentTaskTestService(t, runner, role)
	workspaceState, err := os.MkdirTemp(filepath.Dir(root), "workspace-state-")
	if err != nil {
		t.Fatal(err)
	}
	svc.deps.WorkspaceStateRoot = workspaceState
	t.Cleanup(func() { _ = os.RemoveAll(workspaceState) })
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

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

	task, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{
		AgentName: "builder", Instruction: "hold the writer lease", Isolation: "worktree", Background: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("workspace writer did not start")
	}

	projectRoot, scope, err := svc.workspaceScope(context.Background(), ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID == "" || active.Generation != task.WorkspaceGeneration {
		t.Fatalf("writer lease was not active before cancellation: %+v, %v", active, err)
	}
	if _, err := svc.deps.AgentTasks.Stop(context.Background(), parent, task.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not observe cancellation")
	}
	stillActive, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || stillActive.State != workspace.StateWriting || stillActive.WriterRunID != active.WriterRunID || stillActive.Generation != active.Generation {
		t.Fatalf("cancellation released writer before runner exit: %+v, %v", stillActive, err)
	}

	releaseOnce.Do(func() { close(release) })
	terminal := waitAgentTaskTerminal(t, svc, parent, task.ID)
	settled, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || terminal.Status != agent.DelegationCanceled || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != active.Generation {
		t.Fatalf("writer did not settle after runner exit: task=%+v workspace=%+v err=%v", terminal, settled, err)
	}
	svc.mu.Lock()
	delete(svc.activeRuns, parent.RunID)
	delete(svc.activeRequests, parent.RunID)
	svc.mu.Unlock()
}
