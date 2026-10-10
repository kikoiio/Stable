package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamStopRejectsValidSiblingRunIdentityThenAllowsSessionUser(t *testing.T) {
	root := t.TempDir()
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.Mkdir(goalRoot, 0700); err != nil {
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
	socketDir, err := os.MkdirTemp("", "vs-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketDir, err = filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "conversation.sock")
	service, err := Serve(serviceCtx, Deps{
		ProjectRoot: root, Store: state, SocketPath: socket, PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	created, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	const goalID = "valid-sibling-stop-goal"
	if _, err := state.CreateGoal(ctx, coreGoal(goalID, goalRoot, sessionID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalID, WorkItemID: "owner-item"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalID, WorkItemID: "sibling-item"}
	makeLead := func(runID string, work agent.WorkRef) agent.ExecutionRequest {
		t.Helper()
		if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "valid sibling stop identity fixture",
		}); err != nil {
			t.Fatal(err)
		}
		bounds, marshalErr := json.Marshal(permission.Authority{
			RunID: runID, SessionID: sessionID, GoalID: goalID, WorkItemID: work.WorkItemID, AllowedRoot: goalRoot,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return agent.ExecutionRequest{RunID: runID, Work: work, PermissionBounds: bounds}
	}
	requestA := makeLead("valid-stop-owner-run", workA)
	requestB := makeLead("valid-stop-sibling-run", workB)
	service.mu.Lock()
	service.activeRuns[requestA.RunID] = sessionID
	service.activeRuns[requestB.RunID] = sessionID
	service.activeRequests[requestA.RunID] = requestA
	service.activeRequests[requestB.RunID] = requestB
	service.mu.Unlock()
	if _, scope, actor, err := service.teamOperationScope(ctx, requestB); err != nil || !actor.Lead || scope.WorkItemID != workB.WorkItemID || scope.ProjectRoot != goalRoot {
		t.Fatalf("sibling run fixture is not a valid lead for its own WorkItem/root: scope=%+v actor=%+v err=%v", scope, actor, err)
	}
	team, err := service.CreateTeam(ctx, requestA, "valid-sibling-stop-team")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "valid-sibling-stop-member"
	addTeamMessageMember(t, service, requestA, team.ID, memberID, "reader")
	projection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	const turnID, childRunID = "valid-sibling-stop-turn", "valid-sibling-stop-child"
	turn := sessionlog.TurnFact{
		ID: turnID, MemberID: memberID, RunID: childRunID, TaskID: "valid-sibling-stop-task",
		OriginRunID: requestA.RunID, OriginCallID: "owner-spawn-call", Status: "intent",
	}
	if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: requestA.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: requestA.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	// Keep the accepted child turn queued behind a real scheduler cancel handle;
	// this lets the test inspect state before any child terminal can settle it.
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, childRunID, turnID
	member.Budget.AcceptedTurns++
	member.Revision++
	if err := appendTeamFactLocked(root, sessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: requestA.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	cancelCalls := 0
	service.teamScheduler.mu.Lock()
	service.teamScheduler.active[turnID] = func() { cancelCalls++ }
	service.teamScheduler.activeMember[turnID] = memberID
	service.teamScheduler.activeTeam[turnID] = team.ID
	service.teamScheduler.activeOrigin[turnID] = requestA.RunID
	service.teamScheduler.mu.Unlock()

	beforeProjection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// A real active sibling run cannot attach its WorkItem identity to the
	// session-user stop command. Protocol validation must reject it pre-dispatch.
	if _, err := Request(ctx, socket, ClientMsg{
		Op: "team_member_stop", SessionID: sessionID, TeamID: team.ID, TeamMemberID: memberID,
		RunID: requestB.RunID, GoalID: goalID, WorkItemID: workB.WorkItemID,
	}); err == nil {
		t.Fatal("session-user stop accepted a valid sibling WorkItem run identity")
	}
	afterRejectedProjection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedHistory, err := sessionlog.TeamHistory(root, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedSession, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterRejectedProjection, beforeProjection) || !reflect.DeepEqual(afterRejectedHistory, beforeHistory) || !reflect.DeepEqual(afterRejectedSession.Events, beforeSession.Events) || cancelCalls != 0 {
		t.Fatal("rejected sibling run identity changed team state/facts or canceled the active member")
	}
	service.teamScheduler.mu.Lock()
	_, stillActive := service.teamScheduler.active[turnID]
	service.teamScheduler.mu.Unlock()
	if !stillActive {
		t.Fatal("rejected sibling run identity removed the target from the active scheduler")
	}

	// Stop is intentionally a local session-user action. With RunID/WorkItem
	// omitted, the server derives the authorized scope from the persisted team.
	response, err := Request(ctx, socket, ClientMsg{
		Op: "team_member_stop", SessionID: sessionID, TeamID: team.ID, TeamMemberID: memberID,
	})
	if err != nil || len(response) != 1 || response[0].TeamMember == nil || response[0].TeamMember.Status != teams.MemberStopping {
		t.Fatalf("valid session-user stop response=%+v err=%v; want member stopping", response, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("authorized session-user stop cancel calls=%d, want exactly one", cancelCalls)
	}
	finalProjection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := finalProjection.Members[memberID]; got.Status != teams.MemberStopping || got.TurnID != turnID {
		t.Fatalf("authorized stop changed unexpected member/turn state: %+v", got)
	}
}
