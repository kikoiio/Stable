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

// A storage error can be reported after the TeamTurnTerminal bytes have
// already become durable. The next recovery pass must recognize that fact,
// reconcile the member, and avoid appending a second terminal or rerunning the
// child.
func TestTeamRecoveryConvergesAfterDurableTurnTerminalAppendReturnsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "ambiguous-terminal-parent")
	team, err := service.CreateTeam(t.Context(), request, "ambiguous-terminal")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-ambiguous-terminal", TeamID: team.ID, Name: "reader", AgentName: "explore",
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
		ID: "turn-ambiguous-terminal", MemberID: member.ID, RunID: "child-ambiguous-terminal",
		TaskID: "task-ambiguous-terminal", OriginRunID: request.RunID,
		OriginCallID: "spawn-ambiguous-terminal", Status: "intent",
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn})
	accepted := turn
	accepted.Status = "queued"
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted})
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-ambiguous-terminal", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	var runSeq uint64
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child,
		agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "durable child outcome"}, &runSeq); err != nil {
		t.Fatal(err)
	}

	appendErr := errors.New("injected error reported after durable terminal append")
	appendCalls := 0
	err = recoverTeamRunsWithAppenders(root, appendTeamRecoveryRunTerminal, func(appendRoot, sessionID, teamID string, fact sessionlog.TeamEvent) error {
		appendCalls++
		if fact.Kind != sessionlog.TeamTurnTerminal || fact.Turn == nil || fact.Turn.ID != turn.ID {
			return errors.New("recovery appender received an unexpected team fact")
		}
		if err := appendTeamRecoveryTurnTerminal(appendRoot, sessionID, teamID, fact); err != nil {
			return err
		}
		return appendErr
	})
	if !errors.Is(err, appendErr) {
		t.Fatalf("first recovery error=%v, want injected post-append error", err)
	}
	if appendCalls != 1 {
		t.Fatalf("first recovery terminal append calls=%d, want one", appendCalls)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Turns[turn.ID].Status != string(agent.DelegationSucceeded) || projection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("post-error durable projection turn=%s member=%s, want succeeded/running gap", projection.Turns[turn.ID].Status, projection.Members[member.ID].Status)
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("durable TeamTurnTerminal count=%d, want exactly one", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("child RunTerminal count=%d, want exactly one", got)
	}

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Turns[turn.ID].Status != string(agent.DelegationSucceeded) || projection.Members[member.ID].Status != teams.MemberIdle {
		t.Fatalf("recovered projection turn=%s member=%s, want succeeded/idle", projection.Turns[turn.ID].Status, projection.Members[member.ID].Status)
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("recovered TeamTurnTerminal count=%d, want exactly one", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("recovery changed child RunTerminal count to %d, want one", got)
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
}
