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

func TestTeamRecoveryInterruptsAcceptedTurnWithoutReplayingProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	team, err := service.CreateTeam(t.Context(), request, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-1", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-1", MemberID: member.ID, RunID: "child-run", TaskID: "turn-1", OriginRunID: request.RunID, OriginCallID: "call-spawn", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	turn.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotTurn := projection.Turns[turn.ID]
	gotMember := projection.Members[member.ID]
	if gotTurn.Status != string(agent.DelegationInterrupted) || gotMember.Status != teams.MemberInterrupted {
		t.Fatalf("recovery turn/member = %s/%s, want interrupted/interrupted", gotTurn.Status, gotMember.Status)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var runStart bool
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunStarted {
			continue
		}
		var start sessionlog.RunStarted
		if decodeSessionData(event.Data, &start) == nil && start.RunID == turn.RunID {
			runStart = true
		}
	}
	if !runStart {
		t.Fatal("recovery did not persist child run attribution before terminal outcome")
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery should be idempotent: %v", err)
	}
}

func TestTeamRecoveryDoesNotRunCapacityWaitersAutomatically(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	team, err := service.CreateTeam(t.Context(), request, "capacity-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-waiting", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	member.Status = teams.MemberWaitingCapacity
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", Member: &member}); err != nil {
		t.Fatal(err)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("recovered capacity waiter state=%s, want interrupted", got)
	}
	if len(projection.Turns) != 0 {
		t.Fatalf("capacity recovery fabricated an accepted turn: %+v", projection.Turns)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery should be idempotent: %v", err)
	}
}

func TestUnpublishedTeamAdmissionIsDurablyCompensated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	team, err := service.CreateTeam(context.Background(), request, "publish-failure")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-compensate", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	identity := &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: "turn-compensate", MemberName: member.Name}
	turn := sessionlog.TurnFact{ID: identity.TurnID, MemberID: member.ID, TaskID: identity.TurnID, RunID: "child-run", OriginRunID: request.RunID, OriginCallID: "spawn-call", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{TeamTurn: identity, ParentRunID: request.RunID, BatchID: "batch-compensate", ChildRunID: turn.RunID, Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work}
	scope := teams.Scope{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), ProjectRoot: root}
	runSeq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, scope, request.RunID, "spawn-call", child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.compensateUnpublishedTeamAdmission(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, &runSeq, errors.New("fixture publish failure")); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != string(agent.DelegationInterrupted) {
		t.Fatalf("compensated turn status=%s", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("compensated member status=%s", got)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("recovery after compensation should be idempotent: %v", err)
	}
}

func TestTeamRecoveryReconcilesTerminalTurnBeforeMemberStateGap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	team, err := service.CreateTeam(context.Background(), request, "terminal-gap")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-terminal-gap", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	identity := &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: "turn-terminal-gap", MemberName: member.Name}
	turn := sessionlog.TurnFact{ID: identity.TurnID, MemberID: member.ID, TaskID: identity.TurnID, RunID: "child-terminal-gap", OriginRunID: request.RunID, OriginCallID: "spawn-call", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{TeamTurn: identity, ParentRunID: request.RunID, BatchID: "batch-terminal-gap", ChildRunID: turn.RunID, Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work}
	scope := teams.Scope{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), ProjectRoot: root}
	runSeq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, scope, request.RunID, "spawn-call", child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: "fixture interruption"}, &runSeq); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminal := projection.Turns[turn.ID]
	terminal.Status = string(agent.DelegationInterrupted)
	terminal.Error = "fixture interruption"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: request.RunID, Turn: &terminal}); err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberQueued {
		t.Fatalf("fixture did not leave member terminal gap: %s", got)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("recovery left terminal member in %s", got)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery should be idempotent: %v", err)
	}
}
