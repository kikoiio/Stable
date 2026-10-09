package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type workspaceCreateTrustedRunner struct {
	*workspaceCreateScopeRunner
}

func (r *workspaceCreateTrustedRunner) StartWithExecutorFactory(ctx context.Context, request agent.ExecutionRequest, _ agent.ExecutorFactory) (*agent.RunHandle, error) {
	return r.Start(ctx, request)
}

func TestGoalWorktreeActiveBindingEnterAndExitOverSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(".tmp", "m09-goal-active-binding-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &workspaceCreateTrustedRunner{workspaceCreateScopeRunner: &workspaceCreateScopeRunner{
		started: make(chan agent.ExecutionRequest, 1),
		events:  make(chan agent.ExecutionEvent),
		done:    make(chan agent.RunOutcome, 1),
	}}
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: sandbox.New()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	sessions, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: "goal-active", Objective: "exercise Goal workspace binding", AllowedRoot: formalAbs,
		AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	ownerMessage := ClientMsg{SessionID: sessionID, WorkKind: string(agent.WorkGoal), GoalID: "goal-active", WorkItemID: "item-active"}
	projectRoot, scope, err := service.workspaceScope(ctx, ownerMessage)
	if err != nil {
		t.Fatalf("derive owner Goal scope: %v", err)
	}
	scope.Authority = permission.Authority{
		RunID: "setup-goal-run", SessionID: sessionID, GoalID: scope.Work.GoalID, WorkItemID: scope.Work.WorkItemID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := service.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Create(ctx, scope, "goal-bound-first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, scope, "goal-unbound-second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, first.ID); err != nil {
		t.Fatalf("bind first Goal workspace: %v", err)
	}
	if got, err := manager.Binding(scope); err != nil || got != first.ID {
		t.Fatalf("initial Goal binding=%q err=%v; want %q", got, err, first.ID)
	}

	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ownerWork := scope.Work
	stream, err := OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: runID, Work: ownerWork, Intent: "hold the bound Goal workspace while checking lifecycle changes", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamClosed := false
	t.Cleanup(func() {
		if !streamClosed {
			_ = stream.Cancel(sessionID, runID)
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start bound Goal run: message=%+v err=%v", started, err)
	}
	select {
	case request := <-runner.started:
		if request.RunID != runID || request.Work != ownerWork {
			t.Fatalf("runner started with wrong Goal scope: %+v", request)
		}
	case <-ctx.Done():
		t.Fatal("bound Goal run did not reach runner")
	}

	active, err := manager.Get(ctx, scope, first.ID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != runID || active.Generation == 0 {
		t.Fatalf("Goal run did not acquire the exact bound writer: snapshot=%+v err=%v", active, err)
	}
	if _, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_enter", SessionID: sessionID, WorkKind: string(ownerWork.Kind),
		GoalID: ownerWork.GoalID, WorkItemID: ownerWork.WorkItemID, ID: second.ID,
	}); err == nil {
		t.Fatal("Goal scope entered a different workspace during the active bound run")
	}
	if got, err := manager.Binding(scope); err != nil || got != first.ID {
		t.Fatalf("rejected Goal enter changed binding to %q: err=%v want %q", got, err, first.ID)
	}
	stillActive, err := manager.Get(ctx, scope, first.ID)
	if err != nil || stillActive.State != workspace.StateWriting || stillActive.WriterRunID != runID || stillActive.Generation != active.Generation {
		t.Fatalf("rejected Goal enter changed active writer lease: snapshot=%+v err=%v before=%+v", stillActive, err, active)
	}
	untouched, err := manager.Get(ctx, scope, second.ID)
	if err != nil || untouched.State != workspace.StateReady || untouched.WriterRunID != "" {
		t.Fatalf("rejected Goal enter changed target workspace: snapshot=%+v err=%v", untouched, err)
	}

	exitMessages, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_exit", SessionID: sessionID, WorkKind: string(ownerWork.Kind),
		GoalID: ownerWork.GoalID, WorkItemID: ownerWork.WorkItemID,
	})
	if err != nil || len(exitMessages) != 1 || exitMessages[0].Worktree == nil || exitMessages[0].Worktree.ID != first.ID {
		t.Fatalf("Goal exit did not complete after writer settlement: messages=%+v err=%v", exitMessages, err)
	}
	if got, err := manager.Binding(scope); err != nil || got != "" {
		t.Fatalf("Goal exit left binding %q: err=%v", got, err)
	}
	settled, err := manager.Get(ctx, scope, first.ID)
	if err != nil || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != active.Generation {
		t.Fatalf("Goal exit released binding before/without settling writer: snapshot=%+v err=%v active=%+v", settled, err, active)
	}

	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive Goal run terminal after exit: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("Goal run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	streamClosed = true
}
