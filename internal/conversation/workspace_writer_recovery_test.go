package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

// A cancellation fact can be durable before its run terminal is appended. On
// restart, task recovery must terminalize that run as canceled while workspace
// recovery keeps the matching writer generation fenced for explicit recovery.
func TestCanceledWorkspaceWriterRecoveryRetainsRunAndGenerationBinding(t *testing.T) {
	runner := agentTaskTestRunner(func(context.Context, agent.ChildRunInput) agent.ChildRunResult {
		return agent.ChildRunResult{Status: agent.DelegationCanceled}
	})
	svc, projectRoot, sessionID := newAgentTaskTestService(t, runner, "")
	workspaceState, err := os.MkdirTemp(filepath.Dir(projectRoot), "workspace-recovery-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspaceState) })
	svc.deps.WorkspaceStateRoot = workspaceState

	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}
	root, scope, err := svc.workspaceScope(context.Background(), ClientMsg{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{
		RunID: "lead-run", SessionID: sessionID,
		AllowedRoot: root, FormalRoot: root,
		CandidateRoot: filepath.Join(projectRoot, "candidate"),
	}
	scope.Authority = authority
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), scope, "canceled writer recovery")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireWriter(context.Background(), scope, created.ID, "child-run")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sessionlog.Append(projectRoot, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: "lead-run", WorkKind: string(work.Kind), Intent: "fixture parent",
	}); err != nil {
		t.Fatal(err)
	}
	started := sessionlog.RunStarted{
		RunID: "child-run", WorkKind: string(work.Kind), Intent: "agent task builder",
		AgentTaskID: "task-id", AgentName: "builder", WorkspaceID: lease.WorkspaceID,
		WorkspaceGeneration: lease.Generation, OriginRunID: "lead-run",
	}
	if _, err := sessionlog.Append(projectRoot, sessionID, sessionlog.EventRunStarted, started); err != nil {
		t.Fatal(err)
	}
	delegation := agent.DelegationEvent{BatchID: "cancel-batch", TaskID: "task-id", TaskName: "builder", Status: agent.DelegationCanceled, Error: "canceled by parent"}
	factID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(projectRoot, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: factID, RunID: "child-run", SessionID: sessionID, RunSeq: 1, At: time.Now().UTC(), Kind: "delegation_event", Payload: delegation,
	}); err != nil {
		t.Fatal(err)
	}

	// Closing the workspace manager models a process crash: Close deliberately
	// does not release an active writer without confirmation that it exited.
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.workspaceMu.Lock()
	delete(svc.workspaces, root)
	svc.workspaceMu.Unlock()
	recoveredManager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverAgentTaskRuns(projectRoot); err != nil {
		t.Fatal(err)
	}

	transcript, err := sessionlog.Replay(projectRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil || len(records) != 1 {
		t.Fatalf("recovered task records=%+v err=%v", records, err)
	}
	recoveredTask := taskSnapshot(sessionID, records[0])
	if recoveredTask.Status != agent.DelegationCanceled || recoveredTask.WorkspaceID != lease.WorkspaceID || recoveredTask.WorkspaceGeneration != lease.Generation {
		t.Fatalf("recovered task lost cancellation/workspace fencing facts: %+v", recoveredTask)
	}
	recoveredWorkspace, err := recoveredManager.Get(context.Background(), scope, lease.WorkspaceID)
	if err != nil || recoveredWorkspace.State != workspace.StateInterrupted || recoveredWorkspace.WriterRunID != lease.RunID || recoveredWorkspace.Generation != lease.Generation {
		t.Fatalf("recovery released or advanced interrupted writer: workspace=%+v err=%v lease=%+v", recoveredWorkspace, err, lease)
	}
}
