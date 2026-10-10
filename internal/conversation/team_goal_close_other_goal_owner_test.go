package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamCloseRejectsAnotherGoalOwnerWithoutSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRootA := filepath.Join(root, "goal-a")
	goalRootB := filepath.Join(root, "goal-b")
	for _, dir := range []string{root, goalRootA, goalRootB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "cross Goal team close")
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
	for _, goal := range []struct{ id, root string }{{"goal-close-owner-a", goalRootA}, {"goal-close-owner-b", goalRootB}} {
		if _, err := state.CreateGoal(t.Context(), coreGoal(goal.id, goal.root, session.ID)); err != nil {
			t.Fatal(err)
		}
	}
	makeRequest := func(runID, goalID, workItemID, allowedRoot string) agent.ExecutionRequest {
		t.Helper()
		work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: workItemID}
		request := appendGoalScopeRun(t, root, runID, work)
		request.PermissionBounds, err = json.Marshal(permission.Authority{
			RunID: runID, SessionID: session.ID, GoalID: goalID, WorkItemID: workItemID, AllowedRoot: allowedRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	requestA := makeRequest("goal-close-owner-a-run", "goal-close-owner-a", "item-a", goalRootA)
	requestB := makeRequest("goal-close-owner-b-run", "goal-close-owner-b", "item-b", goalRootB)
	service := &Service{
		deps:           Deps{ProjectRoot: root, Store: state},
		activeRuns:     map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
		activeRequests: map[string]agent.ExecutionRequest{requestA.RunID: requestA, requestB.RunID: requestB},
	}
	for _, fixture := range []struct {
		request agent.ExecutionRequest
		root    string
	}{{requestA, goalRootA}, {requestB, goalRootB}} {
		if _, scope, actor, err := service.teamOperationScope(t.Context(), fixture.request); err != nil || !actor.Lead || scope.GoalID != fixture.request.Work.GoalID || scope.WorkItemID != fixture.request.Work.WorkItemID || scope.ProjectRoot != fixture.root {
			t.Fatalf("fixture is not a valid Goal/WorkItem lead: scope=%+v actor=%+v err=%v", scope, actor, err)
		}
	}
	team, err := service.CreateTeam(t.Context(), requestA, "cross-goal-close")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "cross-goal-close-member"
	addTeamMessageMember(t, service, requestA, team.ID, memberID, "reader")
	turn := sessionlog.TurnFact{
		ID: "cross-goal-close-turn", MemberID: memberID, RunID: "cross-goal-close-child-run",
		TaskID: "cross-goal-close-task", OriginRunID: requestA.RunID, OriginCallID: "cross-goal-close-spawn", Status: "intent",
	}
	if err := appendTeamFactLocked(root, session.ID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: requestA.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, session.ID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: requestA.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, accepted.RunID, accepted.ID
	member.Revision++
	if err := appendTeamFactLocked(root, session.ID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: requestA.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}

	cancelCalls := 0
	service.teamScheduler = &teamScheduler{
		active:       map[string]context.CancelFunc{accepted.ID: func() { cancelCalls++ }},
		activeMember: map[string]string{accepted.ID: memberID},
		activeTeam:   map[string]string{accepted.ID: team.ID},
	}
	beforeProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CloseTeam(t.Context(), requestB, team.ID); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("another valid Goal owner closing team = %v, want ErrPermission", err)
	}
	afterProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistory, beforeHistory) || !reflect.DeepEqual(afterSession.Events, beforeSession.Events) {
		t.Fatal("rejected cross-Goal close changed team projection, history, or session events")
	}
	if got := afterProjection.Members[memberID]; !reflect.DeepEqual(got, beforeProjection.Members[memberID]) || got.Status != teams.MemberQueued {
		t.Fatalf("rejected cross-Goal close changed child member: before=%+v after=%+v", beforeProjection.Members[memberID], got)
	}
	if cancelCalls != 0 {
		t.Fatalf("rejected cross-Goal close canceled child %d times", cancelCalls)
	}

	closing, err := service.CloseTeam(t.Context(), requestA, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("authorized Goal owner close = %+v, %v; want closing while child remains active", closing, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("authorized owner close cancellation calls = %d, want 1", cancelCalls)
	}
}
