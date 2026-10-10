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

func TestTUIUserCanCloseBoundTeamDuringCoordinatorRun(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "m09-tui-coordinator-close-")
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

	childRunner := &forceStopTeamChildRunner{started: make(chan forceStopTeamChildStart, 1), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
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
		select {
		case childRunner.release <- struct{}{}:
		default:
		}
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	initialStream, initial := openTUITestCoordinatorRun(t, reqctx, socket, parentRunner, sessionID, "prepare team close")
	defer initialStream.Close()
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, initial.request.RunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create coordinator-user-close")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted the team")
	}
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI member spawn omitted the member")
	}
	child := receiveForceStopTeamChild(t, childRunner.started)
	if child.input.TeamTurn == nil || child.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("runner received wrong child for %s: %+v", member.ID, child.input.TeamTurn)
	}

	model, enableResult := submitAcceptanceTeamCommand(t, model, "/teams coordinator "+team.ID)
	enabled := acceptanceTeamResponse(t, enableResult, "team_coordinator")
	if !enabled.CoordinatorOn || enabled.CoordinatorTeamID != team.ID {
		t.Fatalf("enable coordinator response=%+v", enabled)
	}
	finishTUITestCoordinatorRun(t, initialStream, initial)
	coordinatorStream, coordinator := openTUITestCoordinatorRun(t, reqctx, socket, parentRunner, sessionID, "close the selected team")
	defer coordinatorStream.Close()
	if !coordinator.request.TeamCoordinator || coordinator.request.TeamCoordinatorTeamID != team.ID {
		t.Fatalf("coordinator binding enabled=%v team=%q, want %q", coordinator.request.TeamCoordinator, coordinator.request.TeamCoordinatorTeamID, team.ID)
	}
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, coordinator.request.RunID, true

	// A coordinator cannot close its own team, even with the valid team ID.
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
		ID: "forged-coordinator-close", Name: "team_close",
		Arguments: []byte(`{"team_id":"` + team.ID + `"}`),
	})
	if directErr != nil || directOutcome.Status != agent.ToolDenied || teamHostCalls != 0 {
		t.Fatalf("direct coordinator close outcome=%+v err=%v host calls=%d; want denial before host", directOutcome, directErr, teamHostCalls)
	}
	select {
	case <-child.ctx.Done():
		t.Fatal("forged coordinator close canceled the child before user close")
	default:
	}

	model, closeResult := submitAcceptanceTeamCommand(t, model, "/teams close "+team.ID)
	closeResponse := acceptanceTeamResponse(t, closeResult, "team_close")
	if closeResponse.Team == nil || closeResponse.Team.Status != teams.TeamClosing {
		t.Fatalf("authorized TUI close response=%+v, want closing until child exit", closeResponse.Team)
	}
	select {
	case <-child.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("authorized TUI/socket close did not cancel the team child")
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Teams[team.ID].Status != teams.TeamClosing || projection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("close marked team/member terminal before child exit: team=%+v member=%+v", projection.Teams[team.ID], projection.Members[member.ID])
	}
	if !model.Pending || model.ActiveRunID != coordinator.request.RunID {
		t.Fatalf("user close changed coordinator run state: pending=%v run=%q", model.Pending, model.ActiveRunID)
	}
	if teamHostCalls != 0 {
		t.Fatalf("direct coordinator close reached TeamToolHost %d times", teamHostCalls)
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
	if projection.Teams[team.ID].Status != teams.TeamClosed || projection.Members[member.ID].Status != teams.MemberStopped {
		t.Fatalf("team/member after child exit = %s/%s, want closed/stopped", projection.Teams[team.ID].Status, projection.Members[member.ID].Status)
	}
	if projection.Turns[child.input.TeamTurn.TurnID].Status != string(agent.DelegationCanceled) {
		t.Fatalf("closed team child turn status=%s, want canceled", projection.Turns[child.input.TeamTurn.TurnID].Status)
	}
	finishTUITestCoordinatorRun(t, coordinatorStream, coordinator)
}
