package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUIInterruptedExportRestartShowsRetainedCandidate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("formal baseline"), 0600); err != nil {
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
	workspaceState := filepath.Join(root, "workspace-state")
	socketParent, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(socketParent, 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(socketParent, "tui-export-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")

	startService := func() (*conversation.Service, context.CancelFunc) {
		t.Helper()
		serviceCtx, cancel := context.WithCancel(ctx)
		service, serveErr := conversation.Serve(serviceCtx, conversation.Deps{
			Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
			SocketPath: socket, PollEvery: time.Hour,
		})
		if serveErr != nil {
			cancel()
			t.Fatal(serveErr)
		}
		return service, cancel
	}

	service, cancel := startService()
	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()

	formal, err = filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectDigest := sha256.Sum256([]byte(filepath.Clean(formal)))
	projectID := "p" + hex.EncodeToString(projectDigest[:16])
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: projectID,
		SessionID: sessionID,
		Work:      agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Authority: permission.Authority{
			RunID: runID, SessionID: sessionID,
			AllowedRoot: formal, FormalRoot: formal,
			CandidateRoot: filepath.Join(formal, ".stable", "candidates"),
		},
	}
	layout, err := workspace.NewLayout(workspaceState, formal, projectID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "interrupted-export")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}

	candidateID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	exportOperationID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	layout, err = workspace.NewLayout(workspaceState, formal, projectID)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := workspace.NewOwnershipStore(layout)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	record, err := ownership.Load(ctx, scope, created.ID)
	if err != nil {
		_ = ownership.Close()
		_ = layout.Close()
		t.Fatal(err)
	}
	record.Snapshot.State = workspace.StateExporting
	record.Snapshot.CandidateID = candidateID
	record.Snapshot.Cursor++
	record.Operation = workspace.Operation{ID: exportOperationID, Kind: "export", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := ownership.Save(ctx, scope, record, record.Snapshot.Generation); err != nil {
		_ = ownership.Close()
		_ = layout.Close()
		t.Fatal(err)
	}
	if err := ownership.Close(); err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	if err := layout.Close(); err != nil {
		t.Fatal(err)
	}

	readThroughTUI := func() workspace.Snapshot {
		t.Helper()
		model := New(socket, formal)
		model.ActiveSession = sessionID
		model.Composer.SetValue("/worktrees get " + created.ID)
		updated, command := model.submitComposer()
		if command == nil {
			t.Fatal("TUI did not dispatch workspace get")
		}
		result, ok := command().(resultMsg)
		if !ok || result.err != nil {
			t.Fatalf("TUI workspace get result=%+v", result)
		}
		updated, _ = updated.(Model).handleResult(result)
		got := updated.(Model)
		if len(got.Worktrees) != 1 {
			t.Fatalf("TUI workspace cache=%+v", got.Worktrees)
		}
		visible := false
		for _, event := range got.Events {
			message, ok := event.Data.(sessionlog.Message)
			if ok && strings.Contains(message.Text, candidateID) && strings.Contains(message.Text, "service restarted during export") {
				visible = true
			}
		}
		if !visible {
			t.Fatalf("TUI transcript did not show retained candidate and reason: %+v", got.Events)
		}
		return got.Worktrees[0]
	}

	for restart := 1; restart <= 2; restart++ {
		service, cancel = startService()
		snapshot := readThroughTUI()
		if snapshot.ID != created.ID || snapshot.State != workspace.StateInterrupted || snapshot.CandidateID != candidateID || snapshot.Error != "service restarted during export; resource retained for explicit recovery" {
			t.Fatalf("restart %d returned unexpected workspace: %+v", restart, snapshot)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		cancel()
	}

	layout, err = workspace.NewLayout(workspaceState, formal, projectID)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err = workspace.NewOwnershipStore(layout)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	stored, err := ownership.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Snapshot.State != workspace.StateInterrupted || stored.Snapshot.CandidateID != candidateID || stored.Operation.ID != exportOperationID || stored.Operation.Phase != "blocked" {
		t.Fatalf("repeated TUI recovery changed retained export journal: %+v", stored)
	}
	if err := ownership.Close(); err != nil {
		t.Fatal(err)
	}
	if err := layout.Close(); err != nil {
		t.Fatal(err)
	}
}
