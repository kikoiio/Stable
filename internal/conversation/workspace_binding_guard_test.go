package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceServiceBlocksEnterAndExitDuringActiveRun(t *testing.T) {
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
	first, err := manager.Create(ctx, scope, "first workspace")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, scope, "second workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, first.ID); err != nil {
		t.Fatalf("enter first workspace while idle: %v", err)
	}

	activeRunID := "active-workspace-run"
	svc.mu.Lock()
	svc.activeRuns[activeRunID] = sessionID
	svc.activeRequests[activeRunID] = agent.ExecutionRequest{
		RunID: activeRunID,
		Work:  scope.Work,
	}
	svc.mu.Unlock()

	if _, err := manager.Exit(ctx, scope); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("exit changed binding during active run: %v", err)
	}
	if _, err := manager.Enter(ctx, scope, second.ID); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("enter changed binding during active run: %v", err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != first.ID {
		t.Fatalf("active run binding=%q err=%v; want %q", bound, err, first.ID)
	}

	svc.mu.Lock()
	delete(svc.activeRuns, activeRunID)
	delete(svc.activeRequests, activeRunID)
	svc.mu.Unlock()
	if _, err := manager.Exit(ctx, scope); err != nil {
		t.Fatalf("exit after run completion: %v", err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != "" {
		t.Fatalf("binding after idle exit=%q err=%v; want empty", bound, err)
	}
}
