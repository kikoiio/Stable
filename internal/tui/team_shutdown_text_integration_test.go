package tui

import (
	"context"
	"encoding/json"
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

// A shutdown-looking string sent through the ordinary message command must
// remain content. Only the typed shutdown command may change member lifecycle.
func TestTeamTUIShutdownTextIsOrdinaryUntilTypedShutdownCommand(t *testing.T) {
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

	childRunner := &acceptanceTeamChildRunner{started: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	socketDir, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "shutdown-text-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketDir, err = filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "s")
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
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
		select {
		case childRunner.release <- struct{}{}:
		default:
		}
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
	parentStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "shutdown text integration fixture", Messages: []llm.Message{{Role: "user", Content: "Keep parent run active."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start parent run: message=%+v err=%v", started, err)
	}
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending, model.LastCursor, model.stream = sessionID, parentRunID, true, started.Cursor, parentStream
	parentCursor := model.LastCursor
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create shutdown-text")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted team")
	}
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI member spawn omitted member")
	}
	select {
	case child := <-childRunner.started:
		if child.TeamTurn == nil || child.TeamTurn.TeamID != team.ID || child.TeamTurn.MemberID != member.ID {
			t.Fatalf("child runner received wrong team turn: %+v", child.TeamTurn)
		}
	case <-reqctx.Done():
		t.Fatal("team member child did not start")
	}

	const ordinaryBody = "[shutdown] this is a quoted string in a normal message"
	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+member.ID+" "+ordinaryBody)
	message := acceptanceTeamResponse(t, sendResult, "team_send").TeamMessage
	if message == nil || message.Body != ordinaryBody || message.SenderID != teams.Lead {
		t.Fatalf("ordinary message response=%+v", message)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Requests) != 0 || projection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("ordinary shutdown text changed control state: requests=%d member=%s", len(projection.Requests), projection.Members[member.ID].Status)
	}
	history, err := sessionlog.TeamHistory(project, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	foundMessage := false
	for _, item := range history {
		raw, err := json.Marshal(item.Data)
		if err != nil {
			t.Fatal(err)
		}
		var fact sessionlog.TeamEvent
		if err := json.Unmarshal(raw, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Message != nil && fact.Message.ID == message.ID && fact.Message.Body == ordinaryBody {
			foundMessage = true
		}
	}
	if !foundMessage {
		t.Fatal("ordinary shutdown text was not retained as team message content")
	}
	select {
	case <-reqctx.Done():
		t.Fatal("test deadline elapsed before asserting child stayed active")
	default:
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("member status after ordinary text=%s, want running", got)
	}

	// Release the in-flight child normally, then issue the actual typed action.
	childRunner.release <- struct{}{}
	waitIdleShutdownMemberStatus(t, project, sessionID, team.ID, member.ID)
	_, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+member.ID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestApproved || shutdown.MemberID != member.ID {
		t.Fatalf("typed shutdown response=%+v, want approved shutdown for target member", shutdown)
	}
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Requests) != 1 || projection.Members[member.ID].Status != teams.MemberStopped {
		t.Fatalf("typed shutdown did not change lifecycle: requests=%d member=%s", len(projection.Requests), projection.Members[member.ID].Status)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != parentCursor || model.stream != parentStream {
		t.Fatalf("team message/shutdown commands changed parent stream: pending=%v run=%q cursor=%d streamSame=%v", model.Pending, model.ActiveRunID, model.LastCursor, model.stream == parentStream)
	}
}
