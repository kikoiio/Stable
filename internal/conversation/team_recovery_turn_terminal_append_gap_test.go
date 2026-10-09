package conversation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamRecoveryRetriesTurnTerminalAppendFailureIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "recovery-turn-terminal-parent")
	team, err := service.CreateTeam(t.Context(), request, "recovery-turn-terminal")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-recovery-turn-terminal", TeamID: team.ID, Name: "reader", AgentName: "explore",
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
		ID: "turn-recovery-turn-terminal", MemberID: member.ID, RunID: "child-recovery-turn-terminal", TaskID: "task-recovery-turn-terminal",
		OriginRunID: request.RunID, OriginCallID: "spawn-recovery-turn-terminal", Status: "intent",
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn})
	accepted := turn
	accepted.Status = "queued"
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted})
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-recovery-turn-terminal", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	var runSeq uint64
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildFinishLocked(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child,
		agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "durable result"}, &runSeq); err != nil {
		t.Fatal(err)
	}

	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Turns[turn.ID].Status != "queued" || before.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("fixture did not preserve the TeamTurnTerminal gap: turn=%s member=%s", before.Turns[turn.ID].Status, before.Members[member.ID].Status)
	}

	appendFailure := errors.New("injected recovery TeamTurnTerminal append failure")
	appendCalls := 0
	err = recoverTeamRunsWithAppenders(root, appendTeamRecoveryRunTerminal, func(appendRoot, sessionID, teamID string, fact sessionlog.TeamEvent) error {
		appendCalls++
		if fact.Kind != sessionlog.TeamTurnTerminal || fact.Turn == nil || fact.Turn.ID != turn.ID {
			return errors.New("recovery appender received an unexpected team fact")
		}
		return appendFailure
	})
	if !errors.Is(err, appendFailure) {
		t.Fatalf("first recovery error=%v, want injected TeamTurnTerminal append failure", err)
	}
	if appendCalls != 1 {
		t.Fatalf("injected TeamTurnTerminal appender called %d times, want exactly once", appendCalls)
	}
	afterFailure, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Turns[turn.ID].Status != "queued" || afterFailure.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("failed recovery published false terminal state: turn=%s member=%s", afterFailure.Turns[turn.ID].Status, afterFailure.Members[member.ID].Status)
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 0 {
		t.Fatalf("failed recovery persisted %d TeamTurnTerminal facts, want zero", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("failed recovery run-terminal count=%d, want existing durable terminal only", got)
	}
	// Recovery receives no runner/provider and therefore cannot execute the
	// child; the durable child terminal is the sole authority for this repair.

	secondAppendCalls := 0
	if err := recoverTeamRunsWithAppenders(root, appendTeamRecoveryRunTerminal, func(appendRoot, sessionID, teamID string, fact sessionlog.TeamEvent) error {
		secondAppendCalls++
		return appendTeamRecoveryTurnTerminal(appendRoot, sessionID, teamID, fact)
	}); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	if secondAppendCalls != 1 {
		t.Fatalf("second recovery TeamTurnTerminal appends=%d, want exactly one", secondAppendCalls)
	}
	recovered, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Turns[turn.ID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("recovered turn status=%s, want succeeded", got)
	}
	if got := recovered.Members[member.ID].Status; got != teams.MemberIdle {
		t.Fatalf("recovered member status=%s, want idle", got)
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("recovered TeamTurnTerminal count=%d, want exactly one", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("recovery changed durable run-terminal count to %d, want one", got)
	}

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("third recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("third recovery appended events: %d -> %d", eventCount, len(transcript.Events))
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("third recovery TeamTurnTerminal count=%d, want exactly one", got)
	}
}

func recoveryTeamTurnTerminalCount(t *testing.T, root, sessionID, teamID, turnID string) int {
	t.Helper()
	history, err := sessionlog.TeamHistory(root, sessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range history {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turnID {
			count++
		}
	}
	return count
}

func recoveryRunTerminalCount(t *testing.T, root, sessionID, runID string) int {
	t.Helper()
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if err := decodeSessionData(event.Data, &runEvent); err != nil {
			t.Fatal(err)
		}
		if runEvent.RunID == runID && runEvent.Kind == string(agent.EventTerminal) {
			count++
		}
	}
	return count
}
