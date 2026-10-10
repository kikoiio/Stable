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

func TestGoalTeamSendSocketRejectsSiblingWorkItemRunWithoutFacts(t *testing.T) {
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
	socketDir, err := os.MkdirTemp(".tmp", "m09-goal-team-message-socket-")
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

	created, err := Request(ctx, service.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	const goalID = "socket-sibling-goal"
	if _, err := state.CreateGoal(ctx, coreGoal(goalID, goalRoot, sessionID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalID, WorkItemID: "socket-item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalID, WorkItemID: "socket-item-b"}
	runA, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runB, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	requestA := agent.ExecutionRequest{RunID: runA, Work: workA}
	requestB := agent.ExecutionRequest{RunID: runB, Work: workB}
	for _, request := range []agent.ExecutionRequest{requestA, requestB} {
		if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: request.RunID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID,
			WorkItemID: request.Work.WorkItemID, Intent: "sibling WorkItem socket message scope",
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.activeRuns[requestA.RunID] = sessionID
	service.activeRuns[requestB.RunID] = sessionID
	service.activeRequests[requestA.RunID] = requestA
	service.activeRequests[requestB.RunID] = requestB
	service.mu.Unlock()

	team, err := service.CreateTeam(ctx, requestA, "socket-item-a-team")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, requestA, team.ID, "socket-item-a-member", "reader")
	beforeProjection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Request(ctx, service.deps.SocketPath, ClientMsg{
		Op: "team_send", SessionID: sessionID, RunID: requestB.RunID, TeamID: team.ID,
		TeamRecipient: "socket-item-a-member", TeamToken: "sibling-workitem-token", Text: "must be rejected",
	})
	if err == nil {
		t.Fatal("Unix socket accepted sibling WorkItem run as the owner team's sender")
	}
	afterProjection, err := sessionlog.ReplayTeams(root, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(root, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistory, beforeHistory) || !reflect.DeepEqual(afterTranscript.Events, beforeTranscript.Events) {
		t.Fatal("rejected sibling WorkItem socket send changed team projection/history or session events")
	}

	ownerMessages, err := Request(ctx, service.deps.SocketPath, ClientMsg{
		Op: "team_send", SessionID: sessionID, RunID: requestA.RunID, TeamID: team.ID,
		TeamRecipient: "socket-item-a-member", TeamToken: "owner-workitem-token", Text: "authorized owner message",
	})
	if err != nil || len(ownerMessages) != 1 || ownerMessages[0].TeamMessage == nil || ownerMessages[0].TeamMessage.Body != "authorized owner message" {
		t.Fatalf("owner WorkItem socket send = %+v, %v", ownerMessages, err)
	}
}
