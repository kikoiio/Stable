package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamRecoveryCompletesDelegationTerminalWithoutRunTerminal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-terminal-gap-parent")
	team, err := service.CreateTeam(t.Context(), request, "run-terminal-gap")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-run-terminal-gap", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	appendFact := func(fact sessionlog.TeamEvent) {
		t.Helper()
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, fact); err != nil {
			t.Fatal(err)
		}
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member})
	turn := sessionlog.TurnFact{
		ID: "turn-run-terminal-gap", MemberID: member.ID, RunID: "child-run-terminal-gap", TaskID: "task-run-terminal-gap",
		OriginRunID: request.RunID, OriginCallID: "spawn-run-terminal-gap", Status: "intent",
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn})
	accepted := turn
	accepted.Status = "queued"
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted})
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-run-terminal-gap", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	var runSeq uint64
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	// Model a crash/write failure after the terminal delegation fact was
	// persisted but before EventTerminal, TeamTurnTerminal, or idle state.
	delegationID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	delegation := sessionlog.AgentTaskDelegation{
		SessionID: request.Work.SessionID, BatchID: child.BatchID, TaskID: child.Task.ID,
		TaskName: member.Name, Status: string(agent.DelegationSucceeded), Summary: "durable child result", UpdatedAt: time.Now().UTC(),
	}
	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: delegationID, RunID: child.ChildRunID, SessionID: request.Work.SessionID,
		RunSeq: runSeq + 1, At: time.Now().UTC(), Kind: string(agent.EventDelegation), Payload: delegation,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Turns[turn.ID].Status != "queued" || before.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("fixture did not preserve the run-terminal gap: turn=%s member=%s", before.Turns[turn.ID].Status, before.Members[member.ID].Status)
	}

	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	terminalAppendFailure := errors.New("injected recovery run-terminal append failure")
	terminalAppendCalls := 0
	err = recoverTeamRunsWithTerminalAppender(root, func(appendRoot, sessionID string, event sessionlog.RunEvent) (sessionlog.Event, error) {
		terminalAppendCalls++
		if event.Kind != string(agent.EventTerminal) {
			return sessionlog.Event{}, errors.New("recovery appender received a nonterminal run event")
		}
		return sessionlog.Event{}, terminalAppendFailure
	})
	if !errors.Is(err, terminalAppendFailure) {
		t.Fatalf("first recovery error=%v, want injected terminal append failure", err)
	}
	if terminalAppendCalls != 1 {
		t.Fatalf("injected terminal appender called %d times, want exactly once", terminalAppendCalls)
	}
	failedProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedProjection.Turns[turn.ID].Status != "queued" || failedProjection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("failed recovery published a false terminal state: turn=%s member=%s", failedProjection.Turns[turn.ID].Status, failedProjection.Members[member.ID].Status)
	}
	failedTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range failedTranscript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if err := decodeSessionData(event.Data, &runEvent); err != nil {
			t.Fatal(err)
		}
		if runEvent.RunID == turn.RunID && runEvent.Kind == string(agent.EventTerminal) {
			t.Fatal("failed recovery persisted a run terminal")
		}
	}
	failedHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range failedHistory {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turn.ID {
			t.Fatal("failed recovery persisted a team turn terminal")
		}
	}
	if runner.count() != 0 {
		t.Fatalf("failed recovery invoked child runner %d times, want zero", runner.count())
	}

	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "m09-run-terminal-gap-")
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove recovery socket directory: %v", err)
		}
	})
	serviceCtx, cancelService := context.WithCancel(context.Background())
	restarted, err := Serve(serviceCtx, Deps{ProjectRoot: root, SocketPath: filepath.Join(socketDir, "recovery.sock"), Delegator: pool})
	if err != nil {
		cancelService()
		pool.Close()
		t.Fatalf("start service and recover missing run terminal: %v", err)
	}
	t.Cleanup(func() {
		cancelService()
		if err := restarted.Close(); err != nil {
			t.Errorf("close restarted service: %v", err)
		}
		pool.Close()
	})
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("recovered team turn status=%s, want succeeded", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberIdle {
		t.Fatalf("recovered member status=%s, want idle", got)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	runTerminalCount, completedRunTerminalCount := 0, 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if err := decodeSessionData(event.Data, &runEvent); err != nil {
			t.Fatal(err)
		}
		if runEvent.RunID != turn.RunID || runEvent.Kind != string(agent.EventTerminal) {
			continue
		}
		runTerminalCount++
		var payload map[string]string
		if err := decodeSessionData(runEvent.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["status"] == string(agent.RunCompleted) {
			completedRunTerminalCount++
		}
	}
	if runTerminalCount != 1 || completedRunTerminalCount != 1 {
		t.Fatalf("recovered run terminal count=%d completed=%d, want exactly one completed", runTerminalCount, completedRunTerminalCount)
	}
	history, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	teamTerminalCount := 0
	for _, event := range history {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turn.ID {
			teamTerminalCount++
		}
	}
	if teamTerminalCount != 1 {
		t.Fatalf("recovered team turn terminal count=%d, want exactly one", teamTerminalCount)
	}
	if runner.count() != 0 {
		t.Fatalf("recovery reran child %d times, want zero", runner.count())
	}
	eventsAfterSuccessfulRetry := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("third recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventsAfterSuccessfulRetry {
		t.Fatalf("third recovery appended events: %d -> %d", eventsAfterSuccessfulRetry, len(transcript.Events))
	}
	if runner.count() != 0 {
		t.Fatalf("third recovery reran child %d times, want zero", runner.count())
	}
}
