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

func TestTeamRecoveryAbortsUnacceptedTurnIntentIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-unaccepted-intent")
	team, err := service.CreateTeam(t.Context(), request, "intent-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-unaccepted-intent", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-unaccepted-intent", MemberID: member.ID, RunID: "child-unaccepted-intent", TaskID: "turn-unaccepted-intent", OriginRunID: request.RunID, OriginCallID: "call-unaccepted-intent", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after durable intent, before pool acceptance or child
	// RunStarted. Recovery must abort this intent without starting a provider.

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != "aborted" {
		t.Fatalf("unaccepted turn status = %q, want aborted", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("member status = %q, want interrupted", got)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	firstRecoveryEventCount := len(transcript.Events)
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventRunStarted:
			var start sessionlog.RunStarted
			if decodeSessionData(event.Data, &start) == nil && start.RunID == turn.RunID {
				t.Fatalf("recovery started child run for unaccepted intent: %+v", start)
			}
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == turn.RunID {
				t.Fatalf("recovery wrote child run event for unaccepted intent: %+v", runEvent)
			}
		}
	}

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != firstRecoveryEventCount {
		t.Fatalf("second recovery appended events: first=%d second=%d", firstRecoveryEventCount, len(transcript.Events))
	}
}

func TestTeamRecoveryRestoresWorktreeAuthorityForAcceptedTurnWithoutRunStarted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-worktree-recovery")
	team, err := service.CreateTeam(t.Context(), request, "worktree-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-worktree-recovery", TeamID: team.ID, Name: "builder", AgentName: "builder", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file", "write_file"}, WorkspaceID: "workspace-worktree-recovery", Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-worktree-recovery", MemberID: member.ID, RunID: "child-worktree-recovery", TaskID: "task-worktree-recovery", WorkspaceID: member.WorkspaceID, WorkspaceGeneration: 7, OriginRunID: request.RunID, OriginCallID: "spawn-worktree-recovery", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart in the durable gap after acceptance but before the
	// child RunStarted fact. Recovery must use the immutable turn authority.
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var recovered sessionlog.RunStarted
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunStarted {
			continue
		}
		var start sessionlog.RunStarted
		if decodeSessionData(event.Data, &start) == nil && start.RunID == turn.RunID {
			recovered = start
			break
		}
	}
	if recovered.RunID != turn.RunID || recovered.WorkspaceID != turn.WorkspaceID || recovered.WorkspaceGeneration != turn.WorkspaceGeneration || recovered.TeamID != team.ID || recovered.TeamMemberID != member.ID || recovered.TeamTurnID != turn.ID {
		t.Fatalf("recovered run start lost worktree authority: %+v", recovered)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Turns[turn.ID].Status != string(agent.DelegationInterrupted) || projection.Members[member.ID].Status != teams.MemberInterrupted {
		t.Fatalf("recovery did not close accepted turn: turn=%+v member=%+v", projection.Turns[turn.ID], projection.Members[member.ID])
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

func TestTeamRecoveryInterruptsMembersWithoutActiveTurns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	team, err := service.CreateTeam(t.Context(), request, "preserve-idle")
	if err != nil {
		t.Fatal(err)
	}
	created := teams.Member{ID: "member-created", TeamID: team.ID, Name: "created", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	idle := teams.Member{ID: "member-idle", TeamID: team.ID, Name: "idle", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	for _, member := range []teams.Member{created, idle} {
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
			t.Fatal(err)
		}
	}
	identity := &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: idle.ID, TurnID: "turn-idle", MemberName: idle.Name}
	turn := sessionlog.TurnFact{ID: identity.TurnID, MemberID: idle.ID, TaskID: identity.TurnID, RunID: "child-idle", OriginRunID: request.RunID, OriginCallID: "spawn-idle", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{TeamTurn: identity, ParentRunID: request.RunID, BatchID: "batch-idle", ChildRunID: turn.RunID, Task: agent.DelegationTask{ID: turn.TaskID, Name: idle.Name, Instruction: "inspect"}, Work: request.Work}
	scope := teams.Scope{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), ProjectRoot: root}
	runSeq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, scope, request.RunID, "spawn-idle", child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, idle.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "done"}, &runSeq); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	finishedTurn := projection.Turns[turn.ID]
	finishedTurn.Status = string(agent.DelegationSucceeded)
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: request.RunID, Turn: &finishedTurn}); err != nil {
		t.Fatal(err)
	}
	idleState := projection.Members[idle.ID]
	idleState.Status = teams.MemberIdle
	idleState.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &idleState}); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[idle.ID].Status; got != teams.MemberIdle {
		t.Fatalf("fixture idle member status=%s", got)
	}
	for range 2 {
		if err := recoverTeamRuns(root); err != nil {
			t.Fatal(err)
		}
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[created.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("created member status after recovery=%s, want interrupted", got)
	}
	if got := projection.Members[idle.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("idle member status after recovery=%s, want interrupted", got)
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
