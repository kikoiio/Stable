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

type forceStopTeamChildStart struct {
	input agent.ChildRunInput
	ctx   context.Context
}

// This runner deliberately does not return when cancellation arrives. That
// keeps the child alive behind a deterministic gate so the test can observe
// the service's stopping state before the real child exit.
type forceStopTeamChildRunner struct {
	started chan forceStopTeamChildStart
	release chan struct{}
}

func (r *forceStopTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- forceStopTeamChildStart{input: input, ctx: ctx}
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: "fixture observed force stop"}
}

func TestTeamTUIForceStopWaitsForChildExitAndPreservesParentStream(t *testing.T) {
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

	childRunner := &forceStopTeamChildRunner{started: make(chan forceStopTeamChildStart, 1), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socketDir, err := os.MkdirTemp("/tmp", "m09-fs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")
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
	parentRequest := agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "force stop fixture", Messages: []llm.Message{{Role: "user", Content: "Keep the parent run active."}},
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, parentRequest)
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
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create force-stop")
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

	model, stopResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" stop "+member.ID)
	stopResponse := acceptanceTeamResponse(t, stopResult, "team_member_stop").TeamMember
	if stopResponse == nil || stopResponse.Status != teams.MemberStopping {
		t.Fatalf("TUI force stop response=%+v, want stopping until child exit", stopResponse)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != parentCursor || model.stream != parentStreamIdentity {
		t.Fatalf("force stop changed parent run/cursor/stream: pending=%v run=%q cursor=%d streamSame=%v", model.Pending, model.ActiveRunID, model.LastCursor, model.stream == parentStreamIdentity)
	}
	select {
	case <-child.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("service did not cancel the child context after the TUI force-stop")
	}
	assertForceStopTeamState(t, project, sessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, teams.MemberStopping, false)

	// The parent run stream remains usable while the child is canceled and held
	// at the gate; the one-shot TUI command must not replace or consume it.
	parentRun.events <- agent.ExecutionEvent{
		ID: "parent-after-force-stop", RunID: parentRunID, SessionID: sessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: agent.EventTextDelta,
	}
	streamMsg, err := parentStream.Receive()
	if err != nil || streamMsg.Type != "run_event" || streamMsg.RunID != parentRunID || streamMsg.RunEvent == nil || streamMsg.RunEvent.ID != "parent-after-force-stop" {
		t.Fatalf("parent stream did not survive team force-stop: message=%+v err=%v", streamMsg, err)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != parentCursor || model.stream != parentStreamIdentity {
		t.Fatal("receiving the surviving parent stream changed TUI state during the one-shot force-stop")
	}

	childRunner.release <- struct{}{}
	waitForForceStopTeamStatus(t, project, sessionID, team.ID, member.ID, teams.MemberStopped)
	assertForceStopTeamState(t, project, sessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, teams.MemberStopped, true)
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
				t.Fatal("force-stopping the child ended the unrelated parent run")
			}
		}
	}
}

func receiveForceStopTeamChild(t *testing.T, started <-chan forceStopTeamChildStart) forceStopTeamChildStart {
	t.Helper()
	select {
	case child := <-started:
		return child
	case <-time.After(5 * time.Second):
		t.Fatal("team child did not reach force-stop gate")
		return forceStopTeamChildStart{}
	}
}

func waitForForceStopTeamStatus(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("member status=%s, want %s", projection.Members[memberID].Status, want)
}

func assertForceStopTeamState(t *testing.T, root, sessionID, teamID, memberID, turnID string, memberStatus teams.MemberStatus, terminal bool) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	if member.Status != memberStatus || member.TurnID != turnID {
		t.Fatalf("replayed member=%+v, want status=%s and turn=%s", member, memberStatus, turnID)
	}
	turn := projection.Turns[turnID]
	if terminal {
		if turn.Status != string(agent.DelegationCanceled) {
			t.Fatalf("turn status after child exit=%s, want canceled", turn.Status)
		}
	} else if turn.Status == string(agent.DelegationCanceled) || turn.Status == string(agent.DelegationSucceeded) || turn.Status == string(agent.DelegationFailed) {
		t.Fatalf("turn became terminal before gated child exit: %+v", turn)
	}
}

func decodeTUIEvent(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
