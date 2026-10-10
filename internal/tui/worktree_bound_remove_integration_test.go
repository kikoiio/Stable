package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUIBoundCleanRemoveRequiresExitThenSucceeds(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-remove-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "conversation.sock")
	runner := &tuiHoldingRunner{}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
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
		stopService()
	})

	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "create a clean workspace for lifecycle validation", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, formal)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, runID, true
	apply := func(line string, wantErr bool) Model {
		t.Helper()
		model.Composer.SetValue(line)
		updated, command := model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not dispatch %s", line)
		}
		result, ok := command().(resultMsg)
		if !ok {
			t.Fatalf("TUI command %s returned unexpected result", line)
		}
		if wantErr && result.err == nil {
			t.Fatalf("%s unexpectedly succeeded: %+v", line, result.msgs)
		}
		if !wantErr && result.err != nil {
			t.Fatalf("%s failed: %v", line, result.err)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		return model
	}
	model = apply("/worktrees create bound-clean", false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].Label != "bound-clean" {
		t.Fatalf("created workspace missing from TUI: %+v", model.Worktrees)
	}
	workspaceID := model.Worktrees[0].ID
	if err := stream.Cancel(sessionID, runID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive lead run cancellation: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("lead run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	model.ActiveRunID, model.Pending, model.stream = "", false, nil
	model = apply("/worktrees enter "+workspaceID, false)
	model = apply("/worktrees remove "+workspaceID, true)
	if model.Err == nil || model.WorktreeDialog != nil {
		t.Fatalf("bound clean remove failure was not surfaced: err=%v status=%q", model.Err, model.Status)
	}
	model = apply("/worktrees get "+workspaceID, false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceID || model.Worktrees[0].State == workspace.StateRemoved {
		t.Fatalf("rejected remove did not retain the bound workspace: %+v", model.Worktrees)
	}

	model = apply("/worktrees exit", false)
	model = apply("/worktrees remove "+workspaceID, false)
	model = apply("/worktrees", false)
	if len(model.Worktrees) != 0 {
		t.Fatalf("workspace remained listed after exit and clean remove: %+v", model.Worktrees)
	}
}
