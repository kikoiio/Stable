package conversation

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestWorkspaceCreateOverSocketRejectsAnotherSessionsActiveRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	socketDir, err := os.MkdirTemp("", "m09-create-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &workspaceCreateScopeRunner{
		started: make(chan agent.ExecutionRequest, 1),
		events:  make(chan agent.ExecutionEvent),
		done:    make(chan agent.RunOutcome, 1),
	}
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	createSession := func() string {
		t.Helper()
		messages, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
		if err != nil || len(messages) != 1 || messages[0].Session == nil {
			t.Fatalf("create session: messages=%+v err=%v", messages, err)
		}
		return messages[0].Session.ID
	}
	ownerSession, otherSession := createSession(), createSession()
	ownerRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: ownerRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: ownerSession},
		Intent: "hold the authorized lead run while checking workspace creation scope", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamClosed := false
	t.Cleanup(func() {
		if !streamClosed {
			_ = stream.Cancel(ownerSession, ownerRunID)
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != ownerRunID {
		t.Fatalf("start owner run: message=%+v err=%v", started, err)
	}
	select {
	case request := <-runner.started:
		if request.RunID != ownerRunID || request.Work.SessionID != ownerSession {
			t.Fatalf("runner started wrong authority: %+v", request)
		}
	case <-ctx.Done():
		t.Fatal("owner lead run did not reach the runner")
	}

	_, err = Request(ctx, socket, ClientMsg{
		Op: "worktree_create", SessionID: otherSession, RunID: ownerRunID, Text: "forged-owner-workspace",
	})
	if err == nil {
		t.Fatal("another session used the owner's active run to create a workspace")
	}
	otherList, err := Request(ctx, socket, ClientMsg{Op: "worktree_list", SessionID: otherSession})
	if err != nil || len(otherList) != 1 || otherList[0].Type != "worktree_list" || len(otherList[0].Worktrees) != 0 {
		t.Fatalf("rejected cross-session create left a workspace: messages=%+v err=%v", otherList, err)
	}

	created, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_create", SessionID: ownerSession, RunID: ownerRunID, Text: "authorized-workspace",
	})
	if err != nil || len(created) != 1 || created[0].Worktree == nil || created[0].Worktree.Label != "authorized-workspace" {
		t.Fatalf("owner create after rejected spoof: messages=%+v err=%v", created, err)
	}
	ownerList, err := Request(ctx, socket, ClientMsg{Op: "worktree_list", SessionID: ownerSession})
	if err != nil || len(ownerList) != 1 || len(ownerList[0].Worktrees) != 1 || ownerList[0].Worktrees[0].ID != created[0].Worktree.ID {
		t.Fatalf("owner workspace was not durably created: messages=%+v err=%v", ownerList, err)
	}

	if err := stream.Cancel(ownerSession, ownerRunID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive owner run cancellation: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("owner run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	streamClosed = true
}

func TestWorkspaceCreateOverSocketRequiresExactGoalWorkItemRunScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	socketDir, err := os.MkdirTemp(".tmp", "m09-goal-create-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &workspaceCreateScopeRunner{
		started: make(chan agent.ExecutionRequest, 1),
		events:  make(chan agent.ExecutionEvent),
		done:    make(chan agent.RunOutcome, 1),
	}
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
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
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	for _, goalID := range []string{"goal-one", "goal-two"} {
		if _, err := db.CreateGoal(ctx, core.Goal{
			ID: goalID, Objective: goalID, AllowedRoot: formalAbs,
			AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: sessionID,
		}); err != nil {
			t.Fatalf("create %s: %v", goalID, err)
		}
	}
	ownerRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ownerWork := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: "goal-one", WorkItemID: "item-a"}
	stream, err := OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: ownerRunID, Work: ownerWork,
		Intent: "hold the authorized Goal WorkItem run while checking workspace creation scope", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamClosed := false
	t.Cleanup(func() {
		if !streamClosed {
			_ = stream.Cancel(sessionID, ownerRunID)
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != ownerRunID {
		t.Fatalf("start owner Goal run: message=%+v err=%v", started, err)
	}
	select {
	case request := <-runner.started:
		if request.RunID != ownerRunID || request.Work != ownerWork {
			t.Fatalf("runner started with wrong Goal authority: %+v", request)
		}
	case <-ctx.Done():
		t.Fatal("owner Goal run did not reach the runner")
	}

	for _, attacker := range []struct {
		name string
		goal string
		item string
	}{
		{name: "other Goal", goal: "goal-two", item: "item-a"},
		{name: "other WorkItem", goal: "goal-one", item: "item-b"},
	} {
		_, err := Request(ctx, socket, ClientMsg{
			Op: "worktree_create", SessionID: sessionID, RunID: ownerRunID,
			WorkKind: string(agent.WorkGoal), GoalID: attacker.goal, WorkItemID: attacker.item,
			Text: "forged-" + attacker.name,
		})
		if err == nil {
			t.Fatalf("%s scope used the owner Goal run to create a workspace", attacker.name)
		}
		listed, listErr := Request(ctx, socket, ClientMsg{
			Op: "worktree_list", SessionID: sessionID,
			WorkKind: string(agent.WorkGoal), GoalID: attacker.goal, WorkItemID: attacker.item,
		})
		if listErr != nil || len(listed) != 1 || listed[0].Type != "worktree_list" || len(listed[0].Worktrees) != 0 {
			t.Fatalf("rejected %s create left a workspace: messages=%+v err=%v", attacker.name, listed, listErr)
		}
	}
	created, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_create", SessionID: sessionID, RunID: ownerRunID,
		WorkKind: string(ownerWork.Kind), GoalID: ownerWork.GoalID, WorkItemID: ownerWork.WorkItemID,
		Text: "authorized-goal-workspace",
	})
	if err != nil || len(created) != 1 || created[0].Worktree == nil || created[0].Worktree.Label != "authorized-goal-workspace" {
		t.Fatalf("exact owner Goal/WorkItem create failed: messages=%+v err=%v", created, err)
	}
	ownerList, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_list", SessionID: sessionID,
		WorkKind: string(ownerWork.Kind), GoalID: ownerWork.GoalID, WorkItemID: ownerWork.WorkItemID,
	})
	if err != nil || len(ownerList) != 1 || len(ownerList[0].Worktrees) != 1 || ownerList[0].Worktrees[0].ID != created[0].Worktree.ID {
		t.Fatalf("owner Goal workspace was not durably created: messages=%+v err=%v", ownerList, err)
	}

	if err := stream.Cancel(sessionID, ownerRunID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive owner Goal run cancellation: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("owner Goal run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	streamClosed = true
}

type workspaceCreateScopeRunner struct {
	started chan agent.ExecutionRequest
	events  chan agent.ExecutionEvent
	done    chan agent.RunOutcome
	once    sync.Once
}

func (r *workspaceCreateScopeRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	r.started <- request
	return &agent.RunHandle{Events: r.events, Done: r.done}, nil
}

func (r *workspaceCreateScopeRunner) Cancel(runID string) error {
	r.once.Do(func() {
		close(r.events)
		r.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
		close(r.done)
	})
	return nil
}
