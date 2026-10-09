package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamTUICloseOverServicePreservesHistoryAndParentRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	childRunner := &acceptanceTeamChildRunner{started: make(chan agent.ChildRunInput, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	shortSocketDir, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shortSocketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp(shortSocketDir, "c-")
	if err != nil {
		t.Fatal(err)
	}
	socket := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	socket, err = filepath.Abs(socket)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parentRequest := agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "team close lifecycle fixture", Messages: []llm.Message{{Role: "user", Content: "Keep the lead run active while closing a team."}},
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if parentRun.request.RunID != parentRunID {
		t.Fatalf("active lead run=%q, want %q", parentRun.request.RunID, parentRunID)
	}
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for active lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model.LastCursor = started.Cursor
	model.stream = parentStream
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create close-lifecycle")
	createResponse := acceptanceTeamResponse(t, createResult, "team_create")
	if createResponse.Team == nil {
		t.Fatalf("create team response=%+v", createResult.msgs)
	}
	teamID := createResponse.Team.ID
	memberID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: memberID, TeamID: teamID, Name: "reader", AgentName: "explore", RoleHash: "fixture", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	memberEventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: memberEventID, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamMemberAdded,
		Revision: 2, ActorID: teams.Lead, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" send "+memberID+" retained before close")
	sendResponse := acceptanceTeamResponse(t, sendResult, "team_send")
	if sendResponse.TeamMessage == nil || sendResponse.TeamMessage.Body != "retained before close" {
		t.Fatalf("pre-close send response=%+v", sendResult.msgs)
	}
	messageID := sendResponse.TeamMessage.ID

	model, closeResult := submitAcceptanceTeamCommand(t, model, "/teams close "+teamID)
	closeResponse := acceptanceTeamResponse(t, closeResult, "team_close")
	if closeResponse.Team == nil || closeResponse.Team.Status != teams.TeamClosed {
		t.Fatalf("TUI close returned=%+v, want closed", closeResponse.Team)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != started.Cursor || model.stream != parentStream {
		t.Fatalf("TUI close changed parent run: pending=%v run=%q cursor=%d stream=%p", model.Pending, model.ActiveRunID, model.LastCursor, model.stream)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Teams[teamID].Status != teams.TeamClosed || projection.Messages[messageID].Body != "retained before close" {
		t.Fatalf("closed team/history not replayable: team=%+v message=%+v", projection.Teams[teamID], projection.Messages[messageID])
	}

	model.Composer.SetValue("/team " + teamID + " send lead rejected after close")
	updated, command := model.submitComposer()
	model, ok := updated.(Model)
	if !ok || command == nil {
		t.Fatalf("post-close send command=%T, want command", updated)
	}
	result, ok := command().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("post-close send result=%+v, want service rejection", result)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != started.Cursor || model.stream != parentStream {
		t.Fatal("rejected post-close send changed parent run state")
	}
	model.Composer.SetValue("/team " + teamID + " spawn late-member explore should-not-run")
	updated, command = model.submitComposer()
	model, ok = updated.(Model)
	if !ok || command == nil {
		t.Fatalf("post-close spawn command=%T, want command", updated)
	}
	result, ok = command().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("post-close spawn result=%+v, want service rejection", result)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != started.Cursor || model.stream != parentStream {
		t.Fatal("rejected post-close spawn changed parent run state")
	}
	select {
	case child := <-childRunner.started:
		t.Fatalf("closed team unexpectedly started child: %+v", child)
	default:
	}
	final, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil || final.Teams[teamID].Status != teams.TeamClosed || len(final.Messages) != 1 || final.Messages[messageID].Body != "retained before close" {
		t.Fatalf("post-close mutations changed durable history: err=%v projection=%+v", err, final)
	}
}
