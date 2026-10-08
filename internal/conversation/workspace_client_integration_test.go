package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceListClientRequestReturnsOwnedPublicSnapshots(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "project.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: filepath.Join(root, "conversation.sock"), PollEvery: time.Hour,
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
	sessions, err := Request(reqctx, svc.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
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
	created, err := manager.Create(ctx, scope, "client visible workspace")
	if err != nil {
		t.Fatal(err)
	}

	listed, err := Request(reqctx, svc.deps.SocketPath, ClientMsg{Op: "worktree_list", SessionID: sessionID, Limit: 10})
	if err != nil || len(listed) != 1 || listed[0].Type != "worktree_list" || len(listed[0].Worktrees) != 1 {
		t.Fatalf("list workspaces: messages=%+v err=%v", listed, err)
	}
	got := listed[0]
	if got.Cursor != created.Cursor || got.Worktrees[0].ID != created.ID || got.Worktrees[0].Label != created.Label || got.Worktrees[0].State != workspace.StateReady {
		t.Fatalf("unexpected client workspace list: %+v", got)
	}
	serialized, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, privatePath := range []string{filepath.Join(root, "workspace-state"), formalAbs, filepath.Join(root, "candidate")} {
		if strings.Contains(string(serialized), privatePath) {
			t.Fatalf("worktree list exposed private path %q: %s", privatePath, serialized)
		}
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalAbs)))
	if scope.ProjectID != "p"+hex.EncodeToString(projectHash[:16]) {
		t.Fatalf("fixture scope project identity drifted: %q", scope.ProjectID)
	}
}
