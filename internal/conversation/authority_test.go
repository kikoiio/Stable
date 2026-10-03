package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestAuthorityConstruction(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	session, err := sessionlog.Create(project, "test")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	goalRoot := filepath.Join(project, "board")
	if err = sessionlogPrepareDir(goalRoot); err != nil {
		t.Fatal(err)
	}
	if _, err = state.CreateGoal(ctx, coreGoal("g", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "g", WorkItemID: "item-1"}, Intent: "repair"}
	a, err := BuildAuthority(ctx, state, project, request, permission.ModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if a.GoalID != "g" || a.WorkItemID != "item-1" || a.FormalRoot != goalRoot || a.CandidateRoot == goalRoot {
		t.Fatalf("authority=%+v", a)
	}
	request.AllowedScope = []string{project}
	if _, err = BuildAuthority(ctx, state, project, request, permission.ModeDefault); err == nil {
		t.Fatal("client scope widened the goal root")
	}
	request.AllowedScope = []string{goalRoot}
	if _, err = BuildAuthority(ctx, state, project, request, permission.ModeDefault); err != nil {
		t.Fatalf("exact scope was rejected: %v", err)
	}
	request.Work.SessionID = "0123456789abcdef0123456789abcdef"
	if _, err = BuildAuthority(ctx, state, project, request, permission.ModeDefault); err == nil {
		t.Fatal("cross-session goal request accepted")
	}
}

func sessionlogPrepareDir(path string) error { return os.MkdirAll(path, 0700) }
func coreGoal(id, root, session string) core.Goal {
	return core.Goal{ID: id, Objective: "goal", AllowedRoot: root, SourceSessionID: session}
}
