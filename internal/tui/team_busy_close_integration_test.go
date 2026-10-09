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

func TestTeamTUICloseWaitsForActiveChildExitAndPreservesParentRun(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tbc-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
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

	childRunner := &forceStopTeamChildRunner{started: make(chan forceStopTeamChildStart, 1), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "conversation.sock")
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
		RunID:    parentRunID,
		Work:     agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent:   "busy team close fixture",
		Messages: []llm.Message{{Role: "user", Content: "Keep the lead run active while closing a busy team."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for active parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model.LastCursor = started.Cursor
	model.stream = parentStream
	parentStreamIdentity, parentCursor := model.stream, model.LastCursor
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create busy-close")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted team")
	}
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI member spawn omitted member")
	}
	child := receiveForceStopTeamChild(t, childRunner.started)
	if child.input.TeamTurn == nil || child.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("fake runner received wrong child turn: %+v", child.input.TeamTurn)
	}

	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+member.ID+" retained while closing")
	message := acceptanceTeamResponse(t, sendResult, "team_send").TeamMessage
	if message == nil || message.Body != "retained while closing" {
		t.Fatalf("pre-close message response=%+v", sendResult.msgs)
	}

	model, closeResult := submitAcceptanceTeamCommand(t, model, "/teams close "+team.ID)
	closing := acceptanceTeamResponse(t, closeResult, "team_close").Team
	if closing == nil || closing.Status != teams.TeamClosing {
		t.Fatalf("TUI close response=%+v, want closing until child exit", closing)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != parentCursor || model.stream != parentStreamIdentity {
		t.Fatalf("close changed parent run/cursor/stream: pending=%v run=%q cursor=%d streamSame=%v", model.Pending, model.ActiveRunID, model.LastCursor, model.stream == parentStreamIdentity)
	}
	select {
	case <-child.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("service did not cancel the active child after the TUI team-close command")
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Teams[team.ID].Status != teams.TeamClosing || projection.Messages[message.ID].Body != message.Body {
		t.Fatalf("closing projection=%+v, want closing team and retained message %q", projection.Teams[team.ID], message.ID)
	}
	closedMember := projection.Members[member.ID]
	closedTurn := projection.Turns[child.input.TeamTurn.TurnID]
	if closedMember.Status != teams.MemberRunning || closedMember.TurnID != child.input.TeamTurn.TurnID {
		t.Fatalf("member became terminal or lost its active turn before child exit: %+v", closedMember)
	}
	if closedTurn.Status == string(agent.DelegationSucceeded) || closedTurn.Status == string(agent.DelegationFailed) || closedTurn.Status == string(agent.DelegationCanceled) {
		t.Fatalf("turn became terminal before gated child exit: %+v", closedTurn)
	}

	parentRun.events <- agent.ExecutionEvent{
		ID: "parent-after-team-close", RunID: parentRunID, SessionID: sessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: agent.EventTextDelta,
	}
	streamMsg, err := parentStream.Receive()
	if err != nil || streamMsg.Type != "run_event" || streamMsg.RunID != parentRunID || streamMsg.RunEvent == nil || streamMsg.RunEvent.ID != "parent-after-team-close" {
		t.Fatalf("parent stream did not survive team close: message=%+v err=%v", streamMsg, err)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != parentCursor || model.stream != parentStreamIdentity {
		t.Fatal("receiving parent stream event changed TUI state during one-shot team close")
	}

	childRunner.release <- struct{}{}
	waitForForceStopTeamStatus(t, project, sessionID, team.ID, member.ID, teams.MemberStopped)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Teams[team.ID].Status == teams.TeamClosed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if projection.Teams[team.ID].Status != teams.TeamClosed {
		t.Fatalf("team status after actual child exit=%s, want closed", projection.Teams[team.ID].Status)
	}
	if projection.Members[member.ID].Status != teams.MemberStopped || projection.Messages[message.ID].Body != message.Body {
		t.Fatalf("closed replay lost terminal member or message: member=%+v message=%+v", projection.Members[member.ID], projection.Messages[message.ID])
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunEvent {
			var runEvent sessionlog.RunEvent
			if err := decodeTUIEvent(event.Data, &runEvent); err != nil {
				t.Fatal(err)
			}
			if runEvent.RunID == parentRunID && runEvent.Kind == string(agent.EventTerminal) {
				t.Fatal("closing the team ended the unrelated parent run")
			}
		}
	}
}
