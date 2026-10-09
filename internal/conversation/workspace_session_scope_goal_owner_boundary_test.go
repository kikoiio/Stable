package conversation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

// A session-level socket request must not inherit ownership of a Goal worktree
// owned by that same session. This protects against callers that omit or forge
// the narrower Goal/WorkItem identity.
func TestWorkspaceSocketSessionScopeCannotMutateGoalOwnedWorkspace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
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
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(".tmp", "m09-session-goal-owner-")
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
	sessions, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create owner session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: "goal-owner", Objective: "workspace ownership boundary", AllowedRoot: formalAbs,
		AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	goalMessage := ClientMsg{SessionID: sessionID, WorkKind: "goal", GoalID: "goal-owner", WorkItemID: "item-owner"}
	projectRoot, ownerScope, err := svc.workspaceScope(ctx, goalMessage)
	if err != nil {
		t.Fatalf("derive Goal owner scope: %v", err)
	}
	ownerScope.Authority = permission.Authority{
		RunID: "fixture-run", SessionID: sessionID, GoalID: ownerScope.Work.GoalID,
		WorkItemID: ownerScope.Work.WorkItemID, AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := svc.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, ownerScope, "goal-owned workspace")
	if err != nil {
		t.Fatalf("create Goal workspace: %v", err)
	}
	if _, err := manager.Enter(ctx, ownerScope, created.ID); err != nil {
		t.Fatalf("bind Goal workspace: %v", err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, ownerScope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "owner.txt")
	if err := os.WriteFile(checkoutFile, []byte("goal owner checkout"), 0600); err != nil {
		t.Fatal(err)
	}
	formalBefore, err := os.ReadFile(formalFile)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, err := expectedWorkspaceCandidateID(ctx, created.ID, created.Generation, formalAbs, layout)
	if err != nil {
		t.Fatalf("compute candidate ID for rejection assertion: %v", err)
	}
	candidatePath := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
	ownerBinding, err := manager.Binding(ownerScope)
	if err != nil || ownerBinding != created.ID {
		t.Fatalf("owner binding=%q err=%v want=%q", ownerBinding, err, created.ID)
	}
	initial, err := manager.Get(ctx, ownerScope, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(op string) {
		t.Helper()
		binding, bindingErr := manager.Binding(ownerScope)
		if bindingErr != nil || binding != ownerBinding {
			t.Fatalf("%s changed Goal binding: got=%q err=%v want=%q", op, binding, bindingErr, ownerBinding)
		}
		got, getErr := manager.Get(ctx, ownerScope, created.ID)
		if getErr != nil || !reflect.DeepEqual(got, initial) {
			t.Fatalf("%s changed Goal owner record: got=%+v err=%v want=%+v", op, got, getErr, initial)
		}
		if data, readErr := os.ReadFile(checkoutFile); readErr != nil || string(data) != "goal owner checkout" {
			t.Fatalf("%s changed owner checkout: data=%q err=%v", op, data, readErr)
		}
		if data, readErr := os.ReadFile(formalFile); readErr != nil || !reflect.DeepEqual(data, formalBefore) {
			t.Fatalf("%s changed formal bytes: data=%q err=%v want=%q", op, data, readErr, formalBefore)
		}
		if _, candidateErr := db.GetCandidate(ctx, candidateID); !errors.Is(candidateErr, sql.ErrNoRows) {
			t.Fatalf("%s created candidate record %s: %v", op, candidateID, candidateErr)
		}
		if _, statErr := os.Lstat(candidatePath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s created candidate tree %s: %v", op, candidatePath, statErr)
		}
	}

	for _, op := range []string{"worktree_get", "worktree_enter", "worktree_keep", "worktree_export", "worktree_remove", "worktree_preview", "worktree_discard_preview"} {
		_, err := Request(ctx, socket, ClientMsg{Op: op, SessionID: sessionID, ID: created.ID})
		if err == nil {
			t.Fatalf("session-only scope accessed Goal-owned workspace through %s", op)
		}
		assertUnchanged(op)
	}
	listed, err := Request(ctx, socket, ClientMsg{Op: "worktree_list", SessionID: sessionID})
	if err != nil || len(listed) != 1 || listed[0].Type != "worktree_list" || len(listed[0].Worktrees) != 0 {
		t.Fatalf("session-only scope exposed Goal workspace: messages=%+v err=%v", listed, err)
	}
	assertUnchanged("worktree_list")
}
