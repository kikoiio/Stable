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
	"stable/internal/teams"
)

func TestTeamSendSocketRejectsAnotherTeamsMemberIDWithoutFacts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(".tmp", "m09-team-message-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	serviceCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service, err := Serve(serviceCtx, Deps{
		ProjectRoot: root, SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
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
	const runID = "same-name-socket-lead"
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: runID, WorkKind: string(agent.WorkSession), Intent: "socket team message scope test",
	}); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}}
	service.mu.Lock()
	service.activeRuns[runID] = sessionID
	service.activeRequests[runID] = request
	service.mu.Unlock()

	createTeam := func(name string) teams.Team {
		t.Helper()
		messages, createErr := Request(ctx, service.deps.SocketPath, ClientMsg{
			Op: "team_create", SessionID: sessionID, RunID: runID, TeamName: name,
		})
		if createErr != nil || len(messages) != 1 || messages[0].Team == nil {
			t.Fatalf("create team %q: messages=%+v err=%v", name, messages, createErr)
		}
		return *messages[0].Team
	}
	teamA := createTeam("socket-team-a")
	teamB := createTeam("socket-team-b")
	addTeamMessageMember(t, service, request, teamA.ID, "socket-member-a", "reader")
	addTeamMessageMember(t, service, request, teamB.ID, "socket-member-b", "reader")

	before, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Request(ctx, service.deps.SocketPath, ClientMsg{
		Op: "team_send", SessionID: sessionID, TeamID: teamA.ID,
		TeamRecipient: "socket-member-b", TeamToken: "foreign-member-id", Text: "must not cross teams",
	})
	if err == nil {
		t.Fatal("Unix socket accepted team B's valid member ID as a team A recipient")
	}
	after, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Events, after.Events) {
		t.Fatalf("rejected socket send appended session facts: before=%d after=%d", len(before.Events), len(after.Events))
	}
	for _, teamID := range []string{teamA.ID, teamB.ID} {
		projection, replayErr := sessionlog.ReplayTeams(root, sessionID, teamID)
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		if len(projection.Messages) != 0 {
			t.Fatalf("rejected socket send changed team %s messages: %+v", teamID, projection.Messages)
		}
	}
}
