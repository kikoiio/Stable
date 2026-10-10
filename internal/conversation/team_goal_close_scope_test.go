package conversation

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamCloseRejectsAnotherWorkItemWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team close scope")
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
	if _, err := state.CreateGoal(t.Context(), coreGoal("goal-close-scope", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-close-scope", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-close-scope", WorkItemID: "item-b"}
	requestA := appendGoalScopeRun(t, root, "goal-close-scope-a", workA)
	requestB := appendGoalScopeRun(t, root, "goal-close-scope-b", workB)
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
	}
	team, err := service.CreateTeam(t.Context(), requestA, "goal-close-scope")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, requestA, team.ID, "member-close-scope", "reader")
	beforeProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CloseTeam(t.Context(), requestB, team.ID); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("another WorkItem closing Goal team = %v, want ErrPermission", err)
	}
	afterProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || afterProjection.Teams[team.ID].Status != teams.TeamOpen {
		t.Fatalf("rejected cross-WorkItem close changed team projection: before=%+v after=%+v", beforeProjection, afterProjection)
	}
	afterHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterHistory) != len(beforeHistory) || len(afterTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatalf("rejected cross-WorkItem close appended facts: team history %d -> %d; session events %d -> %d", len(beforeHistory), len(afterHistory), len(beforeTranscript.Events), len(afterTranscript.Events))
	}
	if got := afterProjection.Members["member-close-scope"].Status; got != teams.MemberCreated {
		t.Fatalf("rejected cross-WorkItem close changed member state to %s", got)
	}
	closed, err := service.CloseTeam(t.Context(), requestA, team.ID)
	if err != nil || closed.Status != teams.TeamClosed {
		t.Fatalf("authorized owner close = %+v, err=%v; want closed", closed, err)
	}
}

func appendGoalScopeRun(t *testing.T, root, runID string, work agent.WorkRef) agent.ExecutionRequest {
	t.Helper()
	if _, err := sessionlog.Append(root, work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal team close scope",
	}); err != nil {
		t.Fatal(err)
	}
	return agent.ExecutionRequest{RunID: runID, Work: work}
}
