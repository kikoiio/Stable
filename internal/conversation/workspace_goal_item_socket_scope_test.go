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

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceSocketScopesGoalAndWorkItemOwnership(t *testing.T) {
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
	socketDir, err := os.MkdirTemp(".tmp", "f1-goal-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
		Runner: &workspaceCreateScopeRunner{
			started: make(chan agent.ExecutionRequest, 1),
			events:  make(chan agent.ExecutionEvent),
			done:    make(chan agent.RunOutcome, 1),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
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
	for _, goalID := range []string{"goal-one", "goal-two"} {
		if _, err := db.CreateGoal(ctx, core.Goal{
			ID: goalID, Objective: goalID, AllowedRoot: formalAbs,
			AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: sessionID,
		}); err != nil {
			t.Fatalf("create %s: %v", goalID, err)
		}
	}

	ownerMessage := ClientMsg{
		SessionID: sessionID, WorkKind: "goal", GoalID: "goal-one", WorkItemID: "item-a",
	}
	projectRoot, ownerScope, err := service.workspaceScope(ctx, ownerMessage)
	if err != nil {
		t.Fatalf("derive owner Goal scope: %v", err)
	}
	ownerScope.Authority = permission.Authority{
		RunID: "scope-fixture-run", SessionID: sessionID, GoalID: ownerScope.Work.GoalID,
		WorkItemID: ownerScope.Work.WorkItemID, AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := service.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, ownerScope, "goal owner workspace")
	if err != nil {
		t.Fatalf("create owner workspace: %v", err)
	}
	mutatorTarget, err := manager.Create(ctx, ownerScope, "unbound goal mutator target")
	if err != nil {
		t.Fatalf("create unbound mutator target: %v", err)
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
	ownerFile := filepath.Join(paths.Checkout, "owner.txt")
	if err := os.WriteFile(ownerFile, []byte("owner checkout"), 0600); err != nil {
		t.Fatal(err)
	}
	mutatorPaths, err := layout.Paths(mutatorTarget.ID)
	if err != nil {
		t.Fatal(err)
	}
	mutatorFile := filepath.Join(mutatorPaths.Checkout, "mutator.txt")
	if err := os.WriteFile(mutatorFile, []byte("unbound owner checkout"), 0600); err != nil {
		t.Fatal(err)
	}
	mutatorCandidateID, err := expectedWorkspaceCandidateID(ctx, mutatorTarget.ID, mutatorTarget.Generation, formalAbs, layout)
	if err != nil {
		t.Fatalf("compute expected export candidate ID: %v", err)
	}
	mutatorCandidateRoot := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", mutatorCandidateID)

	assertOwnerUnchanged := func(op string) {
		t.Helper()
		binding, err := manager.Binding(ownerScope)
		if err != nil || binding != created.ID {
			t.Fatalf("%s changed owner binding: got=%q err=%v want=%q", op, binding, err, created.ID)
		}
		got, err := manager.Get(ctx, ownerScope, created.ID)
		if err != nil || !reflect.DeepEqual(got, created) {
			t.Fatalf("%s changed owner snapshot: got=%+v err=%v want=%+v", op, got, err, created)
		}
		gotMutatorTarget, err := manager.Get(ctx, ownerScope, mutatorTarget.ID)
		if err != nil || !reflect.DeepEqual(gotMutatorTarget, mutatorTarget) {
			t.Fatalf("%s changed unbound owner snapshot: got=%+v err=%v want=%+v", op, gotMutatorTarget, err, mutatorTarget)
		}
		if data, err := os.ReadFile(ownerFile); err != nil || string(data) != "owner checkout" {
			t.Fatalf("%s changed owner checkout: data=%q err=%v", op, data, err)
		}
		if data, err := os.ReadFile(formalFile); err != nil || string(data) != "formal baseline" {
			t.Fatalf("%s changed formal bytes: data=%q err=%v", op, data, err)
		}
		if data, err := os.ReadFile(mutatorFile); err != nil || string(data) != "unbound owner checkout" {
			t.Fatalf("%s changed unbound owner checkout: data=%q err=%v", op, data, err)
		}
		if _, err := db.GetCandidate(ctx, mutatorCandidateID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s created candidate %s: %v", op, mutatorCandidateID, err)
		}
		if _, err := os.Lstat(mutatorCandidateRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s materialized candidate root %s: %v", op, mutatorCandidateRoot, err)
		}
	}

	for _, attacker := range []struct {
		name string
		goal string
		item string
	}{
		{name: "other Goal", goal: "goal-two", item: "item-a"},
		{name: "other WorkItem", goal: "goal-one", item: "item-b"},
	} {
		for _, op := range []string{"worktree_get", "worktree_enter"} {
			_, err := Request(ctx, socket, ClientMsg{
				Op: op, SessionID: sessionID, WorkKind: "goal", GoalID: attacker.goal,
				WorkItemID: attacker.item, ID: created.ID,
			})
			if err == nil {
				t.Fatalf("%s scope accessed owner workspace through %s", attacker.name, op)
			}
			assertOwnerUnchanged(attacker.name + " " + op)
		}
		listed, err := Request(ctx, socket, ClientMsg{
			Op: "worktree_list", SessionID: sessionID, WorkKind: "goal",
			GoalID: attacker.goal, WorkItemID: attacker.item,
		})
		if err != nil || len(listed) != 1 || listed[0].Type != "worktree_list" || len(listed[0].Worktrees) != 0 {
			t.Fatalf("%s scope list exposed owner workspace: messages=%+v err=%v", attacker.name, listed, err)
		}
		assertOwnerUnchanged(attacker.name + " list")
	}

	for _, op := range []string{"worktree_get", "worktree_enter", "worktree_list"} {
		msg := ownerMessage
		msg.Op = op
		if op != "worktree_list" {
			msg.ID = created.ID
		}
		msg.ProjectRoot = filepath.Join(root, "forged-project-root")
		if _, err := Request(ctx, socket, msg); err == nil {
			t.Fatalf("%s accepted client-forged ProjectRoot", op)
		}
		assertOwnerUnchanged("forged ProjectRoot " + op)
	}

	mutators := []ClientMsg{
		{Op: "worktree_keep", ID: mutatorTarget.ID},
		{Op: "worktree_export", ID: mutatorTarget.ID},
		{Op: "worktree_remove", ID: mutatorTarget.ID},
		{Op: "worktree_preview", ID: mutatorTarget.ID},
		{Op: "worktree_discard_preview", ID: mutatorTarget.ID},
		{Op: "worktree_discard", ID: mutatorTarget.ID, DecisionID: mutatorTarget.ID, PreviewDigest: "0000000000000000000000000000000000000000000000000000000000000000", WorktreeGeneration: mutatorTarget.Generation},
		{Op: "worktree_resolve", ID: mutatorTarget.ID, WorktreePreviewID: mutatorTarget.ID, WorktreeGeneration: mutatorTarget.Generation, ConflictChoices: map[string]string{"mutator.txt": workspace.UseWorkspace}},
	}
	for _, attacker := range []struct {
		name string
		goal string
		item string
	}{
		{name: "other Goal", goal: "goal-two", item: "item-a"},
		{name: "other WorkItem", goal: "goal-one", item: "item-b"},
	} {
		for _, operation := range mutators {
			msg := operation
			msg.SessionID = sessionID
			msg.WorkKind = "goal"
			msg.GoalID = attacker.goal
			msg.WorkItemID = attacker.item
			if _, err := Request(ctx, socket, msg); err == nil {
				t.Fatalf("%s scope performed %s on owner workspace", attacker.name, msg.Op)
			}
			assertOwnerUnchanged(attacker.name + " " + msg.Op)
		}
	}
}
