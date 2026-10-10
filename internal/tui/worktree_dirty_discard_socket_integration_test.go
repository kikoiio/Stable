package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUIDirtyDiscardRequiresArmedSocketConfirmation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "formal.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-dirty-discard-")
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
		Intent: "create a workspace for explicit discard confirmation", Model: "fixture",
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
	apply := func(line string) Model {
		t.Helper()
		model.Composer.SetValue(line)
		updated, command := model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not dispatch %s", line)
		}
		result, ok := command().(resultMsg)
		if !ok || result.err != nil {
			t.Fatalf("TUI command %s result=%+v", line, result)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		return model
	}
	model = apply("/worktrees create discard-me")
	if len(model.Worktrees) != 1 || model.Worktrees[0].Label != "discard-me" {
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

	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalAbs)))
	projectID := "p" + hex.EncodeToString(projectHash[:16])
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	paths, err := layout.Paths(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	dirtyFile := filepath.Join(paths.Checkout, "unsaved.txt")
	if err := os.WriteFile(dirtyFile, []byte("must require explicit confirmation"), 0600); err != nil {
		t.Fatal(err)
	}
	model = apply("/worktrees discard " + workspaceID)
	if model.WorktreeDialog == nil || model.WorktreeDialog.Mode != "discard" || model.WorktreeDialog.Snapshot.DiscardID == "" || model.WorktreeDialog.Snapshot.DiscardDigest == "" || model.WorktreeDialog.Snapshot.Generation != model.Worktrees[0].Generation {
		t.Fatalf("TUI did not display a current service discard preview: %+v", model.WorktreeDialog)
	}
	preview := model.WorktreeDialog.Snapshot
	updated, command := model.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if command != nil || model.WorktreeDialog == nil || model.WorktreeDialog.Armed {
		t.Fatal("discard preview was submitted before explicit user arming")
	}
	if got, err := os.ReadFile(dirtyFile); err != nil || string(got) != "must require explicit confirmation" {
		t.Fatalf("unarmed decision changed dirty checkout: got=%q err=%v", got, err)
	}
	updated, command = model.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model = updated.(Model)
	if command != nil || model.WorktreeDialog == nil || !model.WorktreeDialog.Armed {
		t.Fatal("discard preview did not arm after the explicit d key")
	}
	updated, command = model.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if command == nil || model.WorktreeDialog != nil {
		t.Fatal("armed discard did not dispatch to the real conversation socket")
	}
	result, ok := command().(resultMsg)
	if !ok || result.err != nil {
		t.Fatalf("confirmed discard service result=%+v", result)
	}
	updated, _ = model.handleResult(result)
	model = updated.(Model)
	if len(model.Worktrees) != 1 || model.Worktrees[0].State != workspace.StateRemoved || model.Status != "已按用户确认丢弃并删除工作树。" {
		t.Fatalf("TUI did not show confirmed removal: worktrees=%+v status=%q", model.Worktrees, model.Status)
	}
	if _, err := os.Lstat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed discard retained workspace root: %v (preview=%+v)", err, preview)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal baseline" {
		t.Fatalf("discard changed formal bytes: got=%q err=%v", got, err)
	}
}
