package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamStopSocketUsesSessionUserAndPersistedTeamScope(t *testing.T) {
	root := t.TempDir()
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
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

	serviceCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socketDir, err := os.MkdirTemp(os.TempDir(), "m09-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	service, err := Serve(serviceCtx, Deps{
		ProjectRoot: root, Store: state, SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()

	createdA, err := Request(ctx, service.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil || len(createdA) != 1 || createdA[0].Session == nil {
		t.Fatalf("create session A: messages=%+v err=%v", createdA, err)
	}
	createdB, err := Request(ctx, service.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil || len(createdB) != 1 || createdB[0].Session == nil {
		t.Fatalf("create session B: messages=%+v err=%v", createdB, err)
	}
	sessionA, sessionB := createdA[0].Session.ID, createdB[0].Session.ID
	const goalID = "team-stop-session-scope-goal"
	if _, err := state.CreateGoal(ctx, coreGoal(goalID, goalRoot, sessionA)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionA, GoalID: goalID, WorkItemID: "stop-owner-item"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionA, GoalID: goalID, WorkItemID: "stop-sibling-item"}
	requestA := agent.ExecutionRequest{RunID: "stop-owner-run", Work: workA}
	requestB := agent.ExecutionRequest{RunID: "stop-sibling-run", Work: workB}
	for _, request := range []agent.ExecutionRequest{requestA, requestB} {
		if _, err := sessionlog.Append(root, sessionA, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: request.RunID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID,
			WorkItemID: request.Work.WorkItemID, Intent: "team stop session-scoped user fixture",
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.activeRuns[requestA.RunID] = sessionA
	service.activeRuns[requestB.RunID] = sessionA
	service.activeRequests[requestA.RunID] = requestA
	service.activeRequests[requestB.RunID] = requestB
	service.mu.Unlock()

	team, err := service.CreateTeam(ctx, requestA, "stop-session-owner-team")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "stop-session-owner-member"
	addTeamMessageMember(t, service, requestA, team.ID, memberID, "reader")
	projection, err := sessionlog.ReplayTeams(root, sessionA, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	turnID, runID, taskID := "turn-stop-session-owner", "child-stop-session-owner", "task-stop-session-owner"
	turn := sessionlog.TurnFact{
		ID: turnID, MemberID: memberID, RunID: runID, TaskID: taskID,
		OriginRunID: requestA.RunID, OriginCallID: "spawn-stop-session-owner", Status: "intent",
	}
	if err := appendTeamFactLocked(root, sessionA, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: requestA.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, sessionA, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: requestA.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, runID, turnID
	member.Budget.AcceptedTurns++
	member.Revision++
	if err := appendTeamFactLocked(root, sessionA, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: requestA.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}

	cancelCalls := 0
	service.teamScheduler.mu.Lock()
	service.teamScheduler.active[turnID] = func() { cancelCalls++ }
	service.teamScheduler.activeMember[turnID] = memberID
	service.teamScheduler.activeTeam[turnID] = team.ID
	service.teamScheduler.mu.Unlock()

	// The stop socket protocol is intentionally a local, session-scoped user
	// action: it carries no RunID/WorkItemID. The server derives the exact
	// target WorkRef from the persisted team before authorizing the operation.
	userRequest, err := service.teamUserRequest(ctx, sessionA, team.ID)
	if err != nil || userRequest.Work != workA || !userRequest.TeamUser {
		t.Fatalf("server-derived stop actor = %+v, %v; want persisted owner scope %+v", userRequest, err, workA)
	}
	if userRequest.Work == workB {
		t.Fatal("stop actor unexpectedly inherited the sibling active run's WorkRef")
	}

	beforeProjection, err := sessionlog.ReplayTeams(root, sessionA, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, sessionA, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Request(ctx, service.deps.SocketPath, ClientMsg{
		Op: "team_member_stop", SessionID: sessionB, TeamID: team.ID, TeamMemberID: memberID,
	}); err == nil {
		t.Fatal("foreign session user stopped a team bound to session A")
	}
	afterRejectedProjection, err := sessionlog.ReplayTeams(root, sessionA, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedHistory, err := sessionlog.TeamHistory(root, sessionA, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedSession, err := sessionlog.Replay(root, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterRejectedProjection, beforeProjection) || !reflect.DeepEqual(afterRejectedHistory, beforeHistory) || !reflect.DeepEqual(afterRejectedSession.Events, beforeSession.Events) || cancelCalls != 0 {
		t.Fatal("foreign-session stop changed target projection/history/events or canceled its turn")
	}

	response, err := Request(ctx, service.deps.SocketPath, ClientMsg{
		Op: "team_member_stop", SessionID: sessionA, TeamID: team.ID, TeamMemberID: memberID,
	})
	if err != nil || len(response) != 1 || response[0].TeamMember == nil || response[0].TeamMember.Status != teams.MemberStopping {
		t.Fatalf("session A's persisted-scope user stop = %+v, %v; want member stopping", response, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("authorized session user stop cancel calls = %d, want 1", cancelCalls)
	}
	finalProjection, err := sessionlog.ReplayTeams(root, sessionA, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := finalProjection.Members[memberID]; got.Status != teams.MemberStopping || got.TurnID != turnID {
		t.Fatalf("session user stop changed wrong member/turn: %+v", got)
	}
}
