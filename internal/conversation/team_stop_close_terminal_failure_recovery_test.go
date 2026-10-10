package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestStoppedChildTerminalAppendFailureRecoversClosingTeamIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stop-close-terminal-failure-parent")
	team, err := service.CreateTeam(t.Context(), request, "stop-close-terminal-failure")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-stop-close-terminal-failure", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{
		ID: "turn-stop-close-terminal-failure", MemberID: member.ID, RunID: "child-stop-close-terminal-failure",
		TaskID: "task-stop-close-terminal-failure", OriginRunID: request.RunID,
		OriginCallID: "spawn-stop-close-terminal-failure", Status: "intent",
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, turn.RunID, turn.ID
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-stop-close-terminal-failure", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	seq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	if member.Status != teams.MemberRunning {
		t.Fatalf("fixture member status=%s, want running", member.Status)
	}

	cancelCalls := 0
	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	service.lifeCtx = schedulerCtx
	service.teamScheduler = newTeamScheduler(service)
	service.teamScheduler.active[turn.ID] = func() { cancelCalls++ }
	service.teamScheduler.activeMember[turn.ID] = member.ID
	service.teamScheduler.activeTeam[turn.ID] = team.ID
	t.Cleanup(func() {
		cancelScheduler()
		service.teamScheduler.close()
	})
	if stopped, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, member.ID); err != nil || stopped.Status != teams.MemberStopping {
		t.Fatalf("stop active member = %+v, %v; want stopping", stopped, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("force-stop cancellation calls=%d, want one", cancelCalls)
	}
	closing, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("close stopped child team = %+v, %v; want closing while child is active", closing, err)
	}
	if cancelCalls != 2 {
		t.Fatalf("close cancellation calls=%d, want one additional call", cancelCalls)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "child exited before watcher terminal append"}, &seq); err != nil {
		t.Fatal(err)
	}

	appendFailure := errors.New("injected recovery TeamTurnTerminal append failure")
	appendCalls := 0
	err = recoverTeamRunsWithAppenders(root, appendTeamRecoveryRunTerminal, func(appendRoot, sessionID, teamID string, fact sessionlog.TeamEvent) error {
		if teamID != team.ID || fact.Kind != sessionlog.TeamTurnTerminal || fact.Turn == nil || fact.Turn.ID != turn.ID {
			return errors.New("recovery appender received an unexpected team fact")
		}
		appendCalls++
		return appendFailure
	})
	if !errors.Is(err, appendFailure) || appendCalls != 1 {
		t.Fatalf("first recovery = %v with %d terminal appends; want injected failure on exactly one turn", err, appendCalls)
	}
	failed, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Teams[team.ID].Status != teams.TeamClosing || failed.Members[member.ID].Status != teams.MemberStopping || failed.Turns[turn.ID].Status != "queued" {
		t.Fatalf("failed recovery changed pending close/stop state: team=%s member=%s turn=%s", failed.Teams[team.ID].Status, failed.Members[member.ID].Status, failed.Turns[turn.ID].Status)
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 0 {
		t.Fatalf("failed recovery persisted %d turn terminals, want none", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("durable child run terminals=%d, want exactly one", got)
	}

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("retry recovery: %v", err)
	}
	recovered, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Teams[team.ID].Status != teams.TeamClosed || recovered.Members[member.ID].Status != teams.MemberStopped || recovered.Turns[turn.ID].Status != string(agent.DelegationSucceeded) {
		t.Fatalf("recovery did not finish stop and close: team=%s member=%s turn=%s", recovered.Teams[team.ID].Status, recovered.Members[member.ID].Status, recovered.Turns[turn.ID].Status)
	}
	if len(recovered.Requests) != 1 {
		t.Fatalf("recovered shutdown request count=%d, want one", len(recovered.Requests))
	}
	for _, shutdown := range recovered.Requests {
		if shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestApproved || shutdown.MemberID != member.ID {
			t.Fatalf("recovered force-stop decision changed: %+v", shutdown)
		}
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	finalEventCount := len(transcript.Events)
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("recovered team turn terminal count=%d, want one", got)
	}
	if got := recoveryRunTerminalCount(t, root, request.Work.SessionID, turn.RunID); got != 1 {
		t.Fatalf("recovered child run terminal count=%d, want one", got)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != finalEventCount || cancelCalls != 2 {
		t.Fatalf("second recovery appended facts or reran cancel: events %d -> %d, cancel calls=%d", finalEventCount, len(transcript.Events), cancelCalls)
	}
}
