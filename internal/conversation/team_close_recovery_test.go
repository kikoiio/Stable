package conversation

import (
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A process can stop after the child has durably exited but before its watcher
// writes the team turn terminal and member state. A persisted close intent
// must let recovery finish that exact child outcome and close the team.
func TestTeamCloseRecoveryFinishesDurableChildExitGap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "close-recovery-parent")
	team, err := service.CreateTeam(t.Context(), request, "close-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-close-recovery", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	identity := &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: "turn-close-recovery", MemberName: member.Name}
	turn := sessionlog.TurnFact{ID: identity.TurnID, MemberID: member.ID, TaskID: identity.TurnID, RunID: "child-close-recovery", OriginRunID: request.RunID, OriginCallID: "spawn-close-recovery", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{
		TeamTurn: identity, ParentRunID: request.RunID, BatchID: "batch-close-recovery", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	scope := teams.Scope{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), ProjectRoot: root}
	runSeq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, scope, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &runSeq); err != nil {
		t.Fatal(err)
	}
	closing, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("close active team = %+v, err=%v; want closing", closing, err)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "finished before crash"}, &runSeq); err != nil {
		t.Fatal(err)
	}
	// Simulate restart at the child-run terminal / team-turn terminal gap.
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("recovered turn status=%s, want succeeded", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberStopped {
		t.Fatalf("recovered member status=%s, want stopped", got)
	}
	if got := projection.Teams[team.ID].Status; got != teams.TeamClosed {
		t.Fatalf("recovered team status=%s, want closed", got)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminalCount := 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == turn.RunID && runEvent.Kind == string(agent.EventTerminal) {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("child durable terminal count=%d, want exactly one", terminalCount)
	}
	eventCount := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("second recovery appended facts: events %d -> %d", eventCount, len(transcript.Events))
	}
}
