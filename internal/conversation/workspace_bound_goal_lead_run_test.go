package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestBoundGoalWorkItemLeadRunUsesExactWorkspaceBinding(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	goalRoot := filepath.Join(root, "goal-project")
	if err := os.Mkdir(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goalRoot, "board.txt"), []byte("formal goal bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal bound writer")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, goalID := range []string{"goal-one", "goal-two"} {
		if _, err := db.CreateGoal(ctx, core.Goal{
			ID: goalID, Objective: goalID, AllowedRoot: goalRoot,
			AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: session.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner := newLeadWorkspaceRunner(nil)
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	toolSandbox := &leadRunToolSandbox{}
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		deps: Deps{
			Store: db, Runner: runner,
			ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
				Gate: leadRunAllowGate{}, Sandbox: toolSandbox, HelperPath: helperPath,
			}),
			ProjectRoot: root, WorkspaceStateRoot: stateRoot, Model: "fixture",
		},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{},
		runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{},
		workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(goalRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-one", WorkItemID: "item-one"}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{
		SessionID: session.ID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID,
	})
	if err != nil {
		t.Fatal(err)
	}
	goalRoot, err = filepath.Abs(goalRoot)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "setup-run", SessionID: session.ID, GoalID: work.GoalID, WorkItemID: work.WorkItemID,
		AllowedRoot: goalRoot, FormalRoot: goalRoot, CandidateRoot: filepath.Join(goalRoot, "unused-candidate"),
	}
	created, err := manager.Create(ctx, scope, "goal worktree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 16)
	request := agent.ExecutionRequest{
		RunID: "bound-goal-run", Work: work, Intent: "update the goal board",
		Messages: []llm.Message{{Role: "user", Content: "update only the bound workspace"}},
	}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted goal runner did not start")
	}
	runner.mu.Lock()
	gotRequest, factory := runner.request, runner.factory
	runner.mu.Unlock()
	if factory == nil || len(gotRequest.ToolSchemas) != 6 {
		t.Fatalf("goal workspace run lacks trusted writer executor: factory=%T schemas=%+v", factory, gotRequest.ToolSchemas)
	}
	var authority permission.Authority
	if err := json.Unmarshal(gotRequest.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	if authority.GoalID != work.GoalID || authority.WorkItemID != work.WorkItemID || authority.SessionID != session.ID || authority.FormalRoot != goalRoot || authority.AllowedRoot == goalRoot {
		t.Fatalf("goal writer authority=%+v", authority)
	}
	if len(gotRequest.Messages) == 0 || gotRequest.Messages[0].Role != "system" || gotRequest.Messages[0].Content == "" {
		t.Fatalf("bound goal run lacks its workspace system instruction: %+v", gotRequest.Messages)
	}
	executor, err := factory.ForRun(gotRequest)
	if err != nil {
		t.Fatal(err)
	}
	read := llm.ToolUse{ID: "read-goal-board", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"board.txt"}`)}
	if result, err := executor.Execute(ctx, read); err != nil || result.IsError {
		t.Fatalf("goal workspace read result=%+v err=%v", result, err)
	}
	write := llm.ToolUse{ID: "write-goal-board", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"board.txt","content":"workspace goal bytes"}`)}
	result, err := executor.Execute(ctx, write)
	if err != nil || result.IsError {
		t.Fatalf("goal workspace write result=%+v err=%v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(authority.CandidateRoot, "board.txt")); err != nil || string(got) != "workspace goal bytes" {
		t.Fatalf("workspace checkout bytes=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(goalRoot, "board.txt")); err != nil || string(got) != "formal goal bytes" {
		t.Fatalf("formal goal bytes changed=%q err=%v", got, err)
	}
	active, err := manager.Get(ctx, scope, created.ID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != request.RunID || active.Generation == 0 {
		t.Fatalf("active goal workspace=%+v err=%v", active, err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundStart := false
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunStarted {
			continue
		}
		var started sessionlog.RunStarted
		if decodeSessionData(event.Data, &started) == nil && started.RunID == request.RunID {
			foundStart = started.GoalID == work.GoalID && started.WorkItemID == work.WorkItemID && started.WorkspaceID == created.ID && started.WorkspaceGeneration == active.Generation
		}
	}
	if !foundStart {
		t.Fatal("RunStarted did not bind the goal run to its exact workspace generation")
	}

	otherGoal := request
	otherGoal.RunID = "other-goal-run"
	otherGoal.Work = agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-two", WorkItemID: "item-two"}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &otherGoal}, updates); !errors.Is(err, workspace.ErrOwnership) {
		t.Fatalf("different goal borrowed the active workspace binding: %v", err)
	}
	runner.mu.Lock()
	startedRequest := runner.request
	runner.mu.Unlock()
	if startedRequest.RunID != request.RunID {
		t.Fatalf("mismatched goal reached runner: run=%q", startedRequest.RunID)
	}
	for _, disallowed := range []agent.ExecutionRequest{
		{RunID: "team-user-run", Work: work, Intent: "team user", TeamUser: true},
		{RunID: "team-turn-run", Work: work, Intent: "team turn", TeamTurn: &agent.TeamTurnIdentity{TeamID: "team-one", MemberID: "member-one", TurnID: "turn-one"}},
	} {
		if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &disallowed}, updates); !errors.Is(err, workspace.ErrOwnership) {
			t.Fatalf("workspace-bound goal accepted special run %s: %v", disallowed.RunID, err)
		}
	}
	if _, err := manager.Keep(ctx, scope, created.ID); err != nil {
		t.Fatalf("keep goal workspace and stop writer: %v", err)
	}
}
