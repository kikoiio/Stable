package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestGoalTeamQueriesRejectWorkRefForgedOnAnotherActiveRun(t *testing.T) {
	ctx := context.Background()
	projectRoot := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(projectRoot, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(projectRoot, "goal query actor binding")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if _, err := state.CreateGoal(ctx, coreGoal("goal-query-actor-binding", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}

	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-query-actor-binding", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-query-actor-binding", WorkItemID: "item-b"}
	requestA := appendGoalScopeRun(t, projectRoot, "goal-query-actor-run-a", workA)
	requestB := appendGoalScopeRun(t, projectRoot, "goal-query-actor-run-b", workB)
	service := &Service{
		deps:       Deps{ProjectRoot: projectRoot, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
	}
	teamA, err := service.CreateTeam(ctx, requestA, "query-actor-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(ctx, requestB, "query-actor-b")
	if err != nil {
		t.Fatal(err)
	}
	beforeProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The run ID is an active, persisted parent identity, but its WorkRef is
	// forged to another valid WorkItem in the same Goal. Query authorization
	// must bind those two pieces of identity together before exposing teams.
	forged := requestA
	forged.Work = workB
	if listed, err := service.ListTeams(ctx, forged); err == nil || len(listed) != 0 {
		t.Fatalf("forged WorkRef listed teams %+v, err=%v; want rejection", listed, err)
	}
	if got, err := service.GetTeam(ctx, forged, teamB.ID); err == nil || got.ID != "" {
		t.Fatalf("forged WorkRef read team %+v, err=%v; want rejection", got, err)
	}
	afterProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || len(afterTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatal("rejected forged queries changed team projection or appended session facts")
	}

	// Both valid owners can still query their own team after the rejected calls.
	if got, err := service.GetTeam(ctx, requestA, teamA.ID); err != nil || got.ID != teamA.ID {
		t.Fatalf("authorized item-a query = %+v, %v", got, err)
	}
	if got, err := service.GetTeam(ctx, requestB, teamB.ID); err != nil || got.ID != teamB.ID {
		t.Fatalf("authorized item-b query = %+v, %v", got, err)
	}
}
