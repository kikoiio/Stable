package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A normal team turn can exit durably just before its watcher records the
// team-level terminal fact. Startup recovery must reconcile that gap without
// closing the team or invoking the child again.
func TestOpenTeamRecoveryFinishesDurableChildTerminalGap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	original, request := teamServiceFixture(t, root, "open-terminal-gap-parent")
	team, err := original.CreateTeam(t.Context(), request, "open-terminal-gap")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-open-terminal-gap", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-open-terminal-gap", MemberID: member.ID, RunID: "child-open-terminal-gap", TaskID: "task-open-terminal-gap", OriginRunID: request.RunID, OriginCallID: "spawn-open-terminal-gap", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-open-terminal-gap", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	seq := uint64(0)
	if err := original.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := original.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeBudget := before.Members[member.ID].Budget
	if before.Teams[team.ID].Status != teams.TeamOpen || before.Members[member.ID].Status != teams.MemberRunning || beforeBudget.AcceptedTurns != 1 {
		t.Fatalf("fixture before durable finish: team=%s member=%s budget=%+v", before.Teams[team.ID].Status, before.Members[member.ID].Status, beforeBudget)
	}
	if err := original.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "finished before crash"}, &seq); err != nil {
		t.Fatal(err)
	}
	gap, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gap.Turns[turn.ID].Status != "queued" || gap.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("fixture did not leave the run-terminal/team-terminal gap: turn=%s member=%s", gap.Turns[turn.ID].Status, gap.Members[member.ID].Status)
	}

	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "m09-open-gap-")
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	restarted, err := Serve(context.Background(), Deps{ProjectRoot: root, SocketPath: filepath.Join(socketDir, "recovery.sock"), Delegator: pool})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
		t.Fatalf("recovered turn status=%s, want succeeded", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberIdle {
		t.Fatalf("recovered member status=%s, want idle", got)
	}
	if got := projection.Teams[team.ID].Status; got != teams.TeamOpen {
		t.Fatalf("recovered team status=%s, want open", got)
	}
	gotBudget := projection.Members[member.ID].Budget
	if gotBudget.AcceptedTurns != beforeBudget.AcceptedTurns || gotBudget.Elapsed < beforeBudget.Elapsed {
		t.Fatalf("recovery reset or refunded member budget: before=%+v after=%+v", beforeBudget, gotBudget)
	}
	if runner.count() != 0 {
		t.Fatalf("startup recovery reran child %d times, want zero", runner.count())
	}

	facts, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	terminalCount := 0
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turn.ID {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("recovered TeamTurnTerminal count=%d, want exactly one", terminalCount)
	}

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventsAfterFirstRecovery := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventsAfterFirstRecovery {
		t.Fatalf("second recovery appended events: %d -> %d", eventsAfterFirstRecovery, len(transcript.Events))
	}
	if runner.count() != 0 {
		t.Fatalf("second recovery reran child %d times, want zero", runner.count())
	}
}
