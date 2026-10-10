package conversation

import (
	"context"
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

func TestGoalTeamCloseRejectsSiblingWorkItemBeforeCancel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	for _, path := range []string{root, goalRoot} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "goal team close running scope")
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
	const goalID = "goal-close-running-scope"
	if _, err := state.CreateGoal(t.Context(), coreGoal(goalID, goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-owner"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-sibling"}
	requestA := appendGoalScopeRun(t, root, "goal-close-running-owner", workA)
	requestB := appendGoalScopeRun(t, root, "goal-close-running-sibling", workB)
	service := &Service{
		deps:           Deps{ProjectRoot: root, Store: state},
		activeRuns:     map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
		activeRequests: map[string]agent.ExecutionRequest{requestA.RunID: requestA, requestB.RunID: requestB},
	}
	team, err := service.CreateTeam(t.Context(), requestA, "close-running-scope")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "member-close-running-scope"
	addTeamMessageMember(t, service, requestA, team.ID, memberID, "reader")
	turn := sessionlog.TurnFact{
		ID: "turn-close-running-scope", MemberID: memberID, RunID: "child-close-running-scope",
		TaskID: "task-close-running-scope", OriginRunID: requestA.RunID, OriginCallID: "spawn-close-running-scope", Status: "intent",
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
		t.Fatalf("sibling WorkItem close = %v, want ErrPermission", err)
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
		t.Fatal("rejected sibling WorkItem close changed team projection, team history, or session events")
	}
	if cancelCalls != 0 {
		t.Fatalf("rejected sibling WorkItem close canceled child %d times", cancelCalls)
	}

	closing, err := service.CloseTeam(t.Context(), requestA, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("owner close = %+v, %v; want closing while accepted child is active", closing, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("authorized owner close cancellation calls = %d, want 1", cancelCalls)
	}
	finalProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := finalProjection.Members[memberID]; got.Status != teams.MemberQueued || got.TurnID != accepted.ID {
		t.Fatalf("owner close changed active member before child exit: %+v", got)
	}
}
