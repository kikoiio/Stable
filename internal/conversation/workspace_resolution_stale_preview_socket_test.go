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

// A user's conflict choice belongs to the exact preview they saw. If either
// side changes while the decision is open, the socket operation must reject
// that choice and leave the workspace unresolved until a fresh preview.
func TestWorkspaceSocketRejectsResolutionFromStalePreview(t *testing.T) {
	ctx := context.Background()
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
	socketDir, err := os.MkdirTemp(os.TempDir(), "m09-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "conversation.sock")
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
	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sessions, err := Request(reqctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "fixture-run", SessionID: sessionID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := svc.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "stale preview user decision")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formal, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "board.txt")
	if err := os.WriteFile(checkoutFile, []byte("workspace-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal-v1"), 0600); err != nil {
		t.Fatal(err)
	}

	previewMessages, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID})
	if err != nil || len(previewMessages) != 1 || previewMessages[0].Worktree == nil {
		t.Fatalf("preview conflict: messages=%+v err=%v", previewMessages, err)
	}
	preview := previewMessages[0].Worktree
	if preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("unexpected conflict preview: %+v", preview)
	}

	// The formal side changes after the user saw the conflict summary but
	// before the user's per-path choice reaches the service.
	if err := os.WriteFile(formalFile, []byte("formal-v2"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Request(reqctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"board.txt": workspace.UseWorkspace},
	})
	if err == nil {
		t.Fatal("accepted a user resolution after the formal input changed from the preview")
	}
	current, err := manager.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ResolvedCount != 0 || current.CandidateID != "" {
		t.Fatalf("stale user choice persisted or exported: %+v", current)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal-v2" {
		t.Fatalf("stale user choice changed formal content: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(checkoutFile); err != nil || string(got) != "workspace-v1" {
		t.Fatalf("stale user choice changed workspace content: %q err=%v", got, err)
	}

	// Refreshing the preview creates a new user decision boundary; only a
	// choice against that exact preview may proceed to candidate export.
	currentPreviewMessages, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID})
	if err != nil || len(currentPreviewMessages) != 1 || currentPreviewMessages[0].Worktree == nil {
		t.Fatalf("refresh conflict preview: messages=%+v err=%v", currentPreviewMessages, err)
	}
	currentPreview := currentPreviewMessages[0].Worktree
	if currentPreview.PreviewID == preview.PreviewID || currentPreview.FormalDigest == preview.FormalDigest {
		t.Fatalf("changed formal input did not get a fresh preview identity: before=%+v after=%+v", preview, currentPreview)
	}
	resolvedMessages, err := Request(reqctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
		WorktreePreviewID: currentPreview.PreviewID, WorktreeGeneration: currentPreview.Generation,
		ConflictChoices: map[string]string{"board.txt": workspace.UseWorkspace},
	})
	if err != nil || len(resolvedMessages) != 1 || resolvedMessages[0].Worktree == nil || resolvedMessages[0].Worktree.ResolvedCount != 1 {
		t.Fatalf("resolve against refreshed preview: messages=%+v err=%v", resolvedMessages, err)
	}
	exported, err := Request(reqctx, socket, ClientMsg{Op: "worktree_export", SessionID: sessionID, ID: created.ID})
	if err != nil || len(exported) != 1 || exported[0].Worktree == nil || exported[0].Worktree.CandidateID == "" {
		t.Fatalf("export refreshed user resolution: messages=%+v err=%v", exported, err)
	}
	candidateRecord, err := db.GetCandidate(ctx, exported[0].Worktree.CandidateID)
	if err != nil || candidateRecord.Candidate.Status == "accepted" {
		t.Fatalf("export crossed the separate candidate acceptance boundary: candidate=%+v err=%v", candidateRecord, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal-v2" {
		t.Fatalf("export changed formal content before acceptance: %q err=%v", got, err)
	}
}
