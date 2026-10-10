package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceResolutionSocketRejectsAnotherSessionWithoutConsumingPreview(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "board.txt")
	if err := os.WriteFile(formalFile, []byte("baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-resolution-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	svc, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
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
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: ownerSession})
	if err != nil {
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "fixture-run", SessionID: ownerSession, AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := svc.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "socket resolution owner")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte("workspace version"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal version"), 0600); err != nil {
		t.Fatal(err)
	}

	previewMessages, err := Request(ctx, socket, ClientMsg{Op: "worktree_preview", SessionID: ownerSession, ID: created.ID})
	if err != nil || len(previewMessages) != 1 || previewMessages[0].Worktree == nil {
		t.Fatalf("owner conflict preview: messages=%+v err=%v", previewMessages, err)
	}
	preview := previewMessages[0].Worktree
	if preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" || preview.PreviewID == "" {
		t.Fatalf("unexpected owner preview: %+v", preview)
	}

	_, err = Request(ctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: otherSession, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"board.txt": workspace.UseWorkspace},
	})
	if err == nil {
		t.Fatal("another session resolved the owner's workspace conflict over the socket")
	}
	ownerSnapshot, err := manager.Get(ctx, scope, created.ID)
	if err != nil || ownerSnapshot.ResolvedCount != 0 || ownerSnapshot.ResolutionID != "" || ownerSnapshot.CandidateID != "" {
		t.Fatalf("rejected cross-session choice changed workspace state: snapshot=%+v err=%v", ownerSnapshot, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal version" {
		t.Fatalf("rejected choice changed formal bytes: %q err=%v", got, err)
	}

	resolvedMessages, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: ownerSession, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"board.txt": workspace.UseWorkspace},
	})
	if err != nil || len(resolvedMessages) != 1 || resolvedMessages[0].Worktree == nil || resolvedMessages[0].Worktree.ResolvedCount != 1 {
		t.Fatalf("owner resolution after rejected attempt: messages=%+v err=%v", resolvedMessages, err)
	}
	resolved := resolvedMessages[0].Worktree
	if resolved.CandidateID != "" || resolved.State == workspace.StateExported {
		t.Fatalf("user resolution bypassed candidate export/review: %+v", resolved)
	}
}
