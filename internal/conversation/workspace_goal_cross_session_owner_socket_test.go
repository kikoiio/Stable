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

func TestWorkspaceGoalSocketRejectsSiblingSessionWithMatchingGoalIdentity(t *testing.T) {
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
	socketDir, err := os.MkdirTemp(".tmp", "m09-goal-cross-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "conversation.sock")
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
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
	ownerSession, siblingSession := createSession(), createSession()
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	const goalID, workItemID = "goal-cross-session-owner", "item-owner"
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: goalID, Objective: "goal workspace owner boundary", AllowedRoot: formalAbs,
		AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: ownerSession,
	}); err != nil {
		t.Fatal(err)
	}
	ownerMessage := ClientMsg{SessionID: ownerSession, WorkKind: "goal", GoalID: goalID, WorkItemID: workItemID}
	projectRoot, ownerScope, err := service.workspaceScope(ctx, ownerMessage)
	if err != nil {
		t.Fatalf("derive owner Goal scope: %v", err)
	}
	ownerScope.Authority = permission.Authority{
		RunID: "owner-goal-run", SessionID: ownerSession, GoalID: goalID, WorkItemID: workItemID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidates"),
	}
	manager, err := service.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, ownerScope, "owned Goal workspace")
	if err != nil {
		t.Fatalf("create owner workspace: %v", err)
	}
	if _, err := manager.Enter(ctx, ownerScope, created.ID); err != nil {
		t.Fatalf("bind owner workspace: %v", err)
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
	if err := os.WriteFile(checkoutFile, []byte("owner checkout bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	ownerBefore, err := manager.Get(ctx, ownerScope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownerBinding, err := manager.Binding(ownerScope)
	if err != nil || ownerBinding != created.ID {
		t.Fatalf("owner binding=%q err=%v want=%q", ownerBinding, err, created.ID)
	}
	candidateID, err := expectedWorkspaceCandidateID(ctx, created.ID, created.Generation, formalAbs, layout)
	if err != nil {
		t.Fatalf("compute candidate ID: %v", err)
	}
	candidateRoot := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
	formalBefore, err := os.ReadFile(formalFile)
	if err != nil {
		t.Fatal(err)
	}
	forgedRoot := filepath.Join(root, "forged-project-root")
	if err := os.Mkdir(forgedRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(forgedRoot, "forged.txt"), []byte("must not become authority"), 0600); err != nil {
		t.Fatal(err)
	}

	assertOwnerUnchanged := func(op string) {
		t.Helper()
		binding, err := manager.Binding(ownerScope)
		if err != nil || binding != ownerBinding {
			t.Fatalf("%s changed owner binding: got=%q err=%v want=%q", op, binding, err, ownerBinding)
		}
		snapshot, err := manager.Get(ctx, ownerScope, created.ID)
		if err != nil || !reflect.DeepEqual(snapshot, ownerBefore) {
			t.Fatalf("%s changed owner snapshot: got=%+v err=%v want=%+v", op, snapshot, err, ownerBefore)
		}
		if data, err := os.ReadFile(checkoutFile); err != nil || string(data) != "owner checkout bytes" {
			t.Fatalf("%s changed owner checkout: bytes=%q err=%v", op, data, err)
		}
		if data, err := os.ReadFile(formalFile); err != nil || !reflect.DeepEqual(data, formalBefore) {
			t.Fatalf("%s changed formal bytes: bytes=%q err=%v want=%q", op, data, err, formalBefore)
		}
		if _, err := db.GetCandidate(ctx, candidateID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s created candidate record %s: %v", op, candidateID, err)
		}
		if _, err := os.Lstat(candidateRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s materialized candidate root %s: %v", op, candidateRoot, err)
		}
	}

	for _, op := range []string{"worktree_get", "worktree_enter", "worktree_keep", "worktree_export", "worktree_remove", "worktree_preview", "worktree_discard_preview"} {
		_, err := Request(ctx, socket, ClientMsg{
			Op: op, SessionID: siblingSession, WorkKind: "goal", GoalID: goalID, WorkItemID: workItemID,
			ID: created.ID, ProjectRoot: forgedRoot,
		})
		if err == nil {
			t.Fatalf("sibling session with matching Goal identity accessed owner workspace through %s", op)
		}
		assertOwnerUnchanged("cross-session " + op)
	}
	if _, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_list", SessionID: siblingSession, WorkKind: "goal", GoalID: goalID,
		WorkItemID: workItemID, ProjectRoot: forgedRoot,
	}); err == nil {
		t.Fatal("sibling session with matching Goal identity listed the owner workspace")
	}
	assertOwnerUnchanged("cross-session list")
}
