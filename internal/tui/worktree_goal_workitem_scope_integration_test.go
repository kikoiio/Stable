package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/platform/sandbox"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUIGoalWorkItemLifecycleScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp(".tmp", "m09-tui-goal-worktree-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "chat.sock")
	runner := &tuiTrustedHoldingRunner{tuiHoldingRunner: &tuiHoldingRunner{}}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox: sandbox.New(), Gate: tuiBindingAllowGate{},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
		stopService()
	})
	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	for _, goalID := range []string{"goal-scope-owner", "goal-scope-other"} {
		if _, err := db.CreateGoal(ctx, core.Goal{
			ID: goalID, Objective: goalID, AllowedRoot: formalAbs,
			AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: sessionID,
		}); err != nil {
			t.Fatalf("create %s: %v", goalID, err)
		}
	}
	ownerWork := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: "goal-scope-owner", WorkItemID: "item-owner"}
	stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		Work: ownerWork, Intent: "hold an active Goal WorkItem run while creating its workspace", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	runClosed := false
	runID := ""
	t.Cleanup(func() {
		if !runClosed {
			if runID != "" {
				_ = stream.Cancel(sessionID, runID)
			}
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID == "" {
		t.Fatalf("start Goal WorkItem run: message=%+v err=%v", started, err)
	}
	runID = started.RunID
	model := New(socket, formal)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, started.RunID, true
	model, command := submitWorktreeLine(t, model, "/worktrees scope goal "+ownerWork.GoalID+" "+ownerWork.WorkItemID)
	if command != nil {
		t.Fatal("Goal scope selection unexpectedly contacted the socket")
	}
	model, result := runWorktreeTUICommand(t, model, "/worktrees create Goal-owned", false)
	created := model.Worktrees
	if len(created) != 1 || created[0].Label != "Goal-owned" {
		t.Fatalf("TUI did not create a workspace under the active Goal WorkItem: %+v", created)
	}
	workspaceID := created[0].ID
	if len(result.msgs) != 1 || result.msgs[0].Worktree == nil || result.msgs[0].Worktree.SessionID != sessionID {
		t.Fatalf("create response lost owning session: %+v", result.msgs)
	}
	if err := stream.Cancel(sessionID, started.RunID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive Goal run cancellation: %v", receiveErr)
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
	runClosed = true
	model.ActiveRunID, model.Pending = "", false

	model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceID {
		t.Fatalf("Goal TUI scope did not list its workspace: %+v", model.Worktrees)
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees get "+workspaceID, false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceID {
		t.Fatalf("Goal TUI scope did not get its workspace: %+v", model.Worktrees)
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees enter "+workspaceID, false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceID {
		t.Fatalf("Goal TUI scope did not enter its workspace: %+v", model.Worktrees)
	}
	ownerSnapshot := model.Worktrees[0]

	for _, attacker := range []struct {
		name string
		goal string
		item string
	}{
		{name: "wrong WorkItem", goal: ownerWork.GoalID, item: "item-other"},
		{name: "wrong Goal", goal: "goal-scope-other", item: ownerWork.WorkItemID},
	} {
		model, command = submitWorktreeLine(t, model, "/worktrees scope goal "+attacker.goal+" "+attacker.item)
		if command != nil {
			t.Fatalf("select %s scope unexpectedly contacted socket", attacker.name)
		}
		if _, result := runWorktreeTUICommand(t, model, "/worktrees get "+workspaceID, true); result.err == nil {
			t.Fatalf("%s was allowed to read the owner workspace", attacker.name)
		}
		if _, result := runWorktreeTUICommand(t, model, "/worktrees enter "+workspaceID, true); result.err == nil {
			t.Fatalf("%s was allowed to enter the owner workspace", attacker.name)
		}
		model, command = submitWorktreeLine(t, model, "/worktrees scope goal "+ownerWork.GoalID+" "+ownerWork.WorkItemID)
		if command != nil {
			t.Fatal("restore owner scope unexpectedly contacted socket")
		}
		model, _ = runWorktreeTUICommand(t, model, "/worktrees get "+workspaceID, false)
		if len(model.Worktrees) != 1 || !sameWorkspaceSnapshotCore(model.Worktrees[0], ownerSnapshot) {
			t.Fatalf("rejected %s requests changed owner workspace: got=%+v want=%+v", attacker.name, model.Worktrees, ownerSnapshot)
		}
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees exit", false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceID || model.Worktrees[0].State != workspace.StateReady {
		t.Fatalf("Goal WorkItem exit did not preserve and unbind workspace: %+v", model.Worktrees)
	}
}

func sameWorkspaceSnapshotCore(a, b workspace.Snapshot) bool {
	return a.ID == b.ID && a.State == b.State && a.Generation == b.Generation &&
		a.WriterRunID == b.WriterRunID && a.WorkspaceDigest == b.WorkspaceDigest &&
		a.ChangedFiles == b.ChangedFiles && a.ConflictCount == b.ConflictCount
}
