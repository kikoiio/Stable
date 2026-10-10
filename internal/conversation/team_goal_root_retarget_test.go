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

func TestGoalTeamOperationsRejectRetargetedAllowedRoot(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "project")
	rootA := filepath.Join(t.TempDir(), "goal-a-root")
	rootB := filepath.Join(t.TempDir(), "goal-b-root")
	for _, root := range []string{projectRoot, rootA, rootB} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "authorized-goal-root")
	if err := os.Symlink(rootA, alias); err != nil {
		t.Fatal(err)
	}

	session, err := sessionlog.Create(projectRoot, "team goal root retarget")
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
	if _, err := state.CreateGoal(t.Context(), coreGoal("goal-root-retarget", alias, session.ID)); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-root-retarget", WorkItemID: "item-root-retarget"}
	request := appendGoalScopeRun(t, projectRoot, "goal-root-retarget-run", work)
	service := &Service{
		deps:       Deps{ProjectRoot: projectRoot, Store: state},
		activeRuns: map[string]string{request.RunID: session.ID},
	}
	team, err := service.CreateTeam(t.Context(), request, "root-retarget")
	if err != nil {
		t.Fatal(err)
	}
	if team.Scope.ProjectRoot != rootA {
		t.Fatalf("team project root = %q, want canonical root %q", team.Scope.ProjectRoot, rootA)
	}

	member := teams.Member{
		ID: "member-root-retarget", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "fixture-role-hash", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(projectRoot, session.ID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{
		ID: "turn-root-retarget", MemberID: member.ID, RunID: "child-root-retarget",
		TaskID: "task-root-retarget", OriginRunID: request.RunID, OriginCallID: "spawn-root-retarget", Status: "intent",
	}
	if err := appendTeamFactLocked(projectRoot, session.ID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(projectRoot, session.ID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted,
	}); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(projectRoot, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	member.Status, member.TurnID, member.RunID = teams.MemberQueued, turn.ID, turn.RunID
	member.Revision++
	if err := appendTeamFactLocked(projectRoot, session.ID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}

	cancelCalls := 0
	service.teamScheduler = &teamScheduler{
		active:       map[string]context.CancelFunc{turn.ID: func() { cancelCalls++ }},
		activeMember: map[string]string{turn.ID: member.ID},
		activeTeam:   map[string]string{turn.ID: team.ID},
	}
	beforeProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(projectRoot, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Retarget the persisted Goal path after the team was created. Its stored
	// scope must remain bound to root A instead of following the alias to root B.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rootB, alias); err != nil {
		t.Fatal(err)
	}

	if _, err := service.GetTeam(t.Context(), request, team.ID); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("GetTeam after AllowedRoot retarget = %v, want ErrPermission", err)
	}
	if _, err := service.CloseTeam(t.Context(), request, team.ID); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("CloseTeam after AllowedRoot retarget = %v, want ErrPermission", err)
	}
	if _, err := service.StopTeamMember(t.Context(), session.ID, team.ID, member.ID); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("StopTeamMember after AllowedRoot retarget = %v, want ErrPermission", err)
	}

	afterProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(projectRoot, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistory, beforeHistory) || !reflect.DeepEqual(afterTranscript.Events, beforeTranscript.Events) {
		t.Fatal("rejected root-retargeted operations changed team projection, team history, or session facts")
	}
	if got := afterProjection.Members[member.ID]; got.Status != teams.MemberQueued || got.TurnID != turn.ID {
		t.Fatalf("rejected root-retargeted operations changed member state: %+v", got)
	}
	if cancelCalls != 0 {
		t.Fatalf("rejected root-retargeted operations canceled child %d times", cancelCalls)
	}
}
