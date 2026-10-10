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
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTUIUserCanStopBoundTeamDuringCoordinatorRun(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "m09-tui-coordinator-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
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

	childRunner := &forceStopTeamChildRunner{started: make(chan forceStopTeamChildStart, 2), release: make(chan struct{}, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 2, 2
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &tuiCoordinatorRecordingRunner{
		defaults: []llm.ToolSchema{{Name: "read_file"}, {Name: "team_list"}, {Name: "team_send"}, {Name: "team_task_update"}},
		started:  make(chan *tuiCoordinatorRunCapture, 3),
	}
	socket := filepath.Join(root, "s")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: agentcatalog.New("", ""),
		ToolSchemas:  parentRunner.defaults,
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
	t.Cleanup(func() {
		for range 2 {
			childRunner.release <- struct{}{}
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	initialStream, initial := openTUITestCoordinatorRun(t, reqctx, socket, parentRunner, sessionID, "prepare team children")
	defer initialStream.Close()
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, initial.request.RunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create coordinator-user-stop")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted the team")
	}
	members := make([]forceStopTeamChildStart, 0, 2)
	for _, name := range []string{"reader-a", "reader-b"} {
		_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn "+name+" explore inspect the assigned area")
		spawned := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
		if spawned == nil {
			t.Fatalf("TUI spawn omitted member %s", name)
		}
		child := receiveForceStopTeamChild(t, childRunner.started)
		if child.input.TeamTurn == nil || child.input.TeamTurn.MemberID != spawned.ID {
			t.Fatalf("runner received wrong child for %s: %+v", name, child.input.TeamTurn)
		}
		members = append(members, child)
	}

	model, enableResult := submitAcceptanceTeamCommand(t, model, "/teams coordinator "+team.ID)
	enabled := acceptanceTeamResponse(t, enableResult, "team_coordinator")
	if !enabled.CoordinatorOn || enabled.CoordinatorTeamID != team.ID {
		t.Fatalf("enable coordinator response=%+v", enabled)
	}
	finishTUITestCoordinatorRun(t, initialStream, initial)
	coordinatorStream, coordinator := openTUITestCoordinatorRun(t, reqctx, socket, parentRunner, sessionID, "coordinate the selected team")
	defer coordinatorStream.Close()
	if !coordinator.request.TeamCoordinator || coordinator.request.TeamCoordinatorTeamID != team.ID {
		t.Fatalf("coordinator run binding enabled=%v team=%q, want %q", coordinator.request.TeamCoordinator, coordinator.request.TeamCoordinatorTeamID, team.ID)
	}
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, coordinator.request.RunID, true

	// A coordinator model cannot invoke the local force-stop tool directly,
	// even when given the valid selected-team/member identifiers.
	teamHost := &agent.TeamToolHost{}
	teamHostCalls := 0
	teamHost.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		teamHostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded}, nil
	})
	executor, err := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{}, execution.WithTeamToolHost(teamHost)).ForRun(coordinator.request)
	if err != nil {
		t.Fatalf("construct coordinator executor: %v", err)
	}
	directOutcome, directErr := executor.Execute(reqctx, llm.ToolUse{
		ID: "forged-coordinator-stop", Name: "team_member_stop",
		Arguments: []byte(`{"team_id":"` + team.ID + `","member_id":"` + members[0].input.TeamTurn.MemberID + `"}`),
	})
	if directErr != nil || directOutcome.Status != agent.ToolDenied || teamHostCalls != 0 {
		t.Fatalf("direct coordinator stop outcome=%+v err=%v host calls=%d; want denial before host", directOutcome, directErr, teamHostCalls)
	}
	select {
	case <-members[0].ctx.Done():
		t.Fatal("forged coordinator tool canceled target before the user stop request")
	default:
	}

	model, stopResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" stop "+members[0].input.TeamTurn.MemberID)
	stopResponse := acceptanceTeamResponse(t, stopResult, "team_member_stop").TeamMember
	if stopResponse == nil || stopResponse.Status != teams.MemberStopping {
		t.Fatalf("TUI user stop response=%+v, want stopping", stopResponse)
	}
	select {
	case <-members[0].ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("user stop through the TUI/socket did not cancel the selected team child")
	}
	select {
	case <-members[1].ctx.Done():
		t.Fatal("stopping one member also canceled its sibling child")
	default:
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[members[0].input.TeamTurn.MemberID].Status; got != teams.MemberStopping {
		t.Fatalf("stopped target status=%s, want stopping", got)
	}
	if got := projection.Members[members[1].input.TeamTurn.MemberID].Status; got != teams.MemberRunning {
		t.Fatalf("untargeted sibling status=%s, want running", got)
	}
	if teamHostCalls != 0 {
		t.Fatalf("coordinator direct stop reached user host %d times", teamHostCalls)
	}
	if !model.Pending || model.ActiveRunID != coordinator.request.RunID {
		t.Fatalf("TUI user stop replaced the active coordinator run: pending=%v active=%q", model.Pending, model.ActiveRunID)
	}

	coordinator.events <- agent.ExecutionEvent{ID: "coordinator-after-user-stop", RunID: coordinator.request.RunID, SessionID: sessionID, RunSeq: 1, At: time.Now().UTC(), Kind: agent.EventTextDelta}
	streamMsg, err := coordinatorStream.Receive()
	if err != nil || streamMsg.Type != "run_event" || streamMsg.RunID != coordinator.request.RunID || streamMsg.RunEvent == nil || streamMsg.RunEvent.ID != "coordinator-after-user-stop" {
		t.Fatalf("coordinator stream did not survive user stop: message=%+v err=%v", streamMsg, err)
	}
	finishTUITestCoordinatorRun(t, coordinatorStream, coordinator)
}
