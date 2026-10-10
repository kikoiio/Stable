package conversation

import (
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A child may have durably written its terminal run event when the process
// exits, before the watcher records the team turn terminal and stopped member
// state. Recovery must honor the already approved member stop in that gap.
func TestStoppedTeamTurnRecoveryPreservesStopAfterChildTerminal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stop-terminal-gap-parent")
	team, err := service.CreateTeam(t.Context(), request, "stop-terminal-gap")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-stop-terminal-gap", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-stop-terminal-gap", MemberID: member.ID, RunID: "child-stop-terminal-gap", TaskID: "task-stop-terminal-gap", OriginRunID: request.RunID, OriginCallID: "spawn-stop-terminal-gap", Status: "intent"}
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
		ParentRunID: request.RunID, BatchID: "batch-stop-terminal-gap", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	seq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	shutdown, err := service.createTeamRequest(root, team, request.RunID, teams.Lead, member.ID, teams.RequestShutdown, "")
	if err != nil {
		t.Fatal(err)
	}
	team.Revision++
	shutdown.Status = teams.RequestApproved
	shutdown.Revision++
	if err := service.appendTeamRequest(root, team, request.RunID, "service", sessionlog.TeamRequestResponded, shutdown); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	member = projection.Members[member.ID]
	member.Status = teams.MemberStopping
	member.Revision++
	if err := service.appendTeamMemberState(root, team, request.RunID, "service", member); err != nil {
		t.Fatal(err)
	}

	// Simulate process exit after the child terminal was persisted but before
	// the watcher writes TeamTurnTerminal or MemberStopped.
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	if projection.Members[member.ID].Status != teams.MemberStopping {
		t.Fatalf("fixture member status=%s, want stopping", projection.Members[member.ID].Status)
	}
	if err := service.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "finished before crash"}, &seq); err != nil {
		t.Fatal(err)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	assertRecoveredStoppedTerminalGap(t, root, request.Work.SessionID, team.ID, member.ID, turn.ID, shutdown.ID)

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventsAfterFirstRecovery := len(transcript.Events)
	if got := countTeamTurnTerminals(t, transcript.Events, turn.ID); got != 1 {
		t.Fatalf("recovered team turn terminal count=%d, want one", got)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventsAfterFirstRecovery || countTeamTurnTerminals(t, transcript.Events, turn.ID) != 1 {
		t.Fatalf("second recovery changed terminal journal: events %d -> %d", eventsAfterFirstRecovery, len(transcript.Events))
	}
	assertRecoveredStoppedTerminalGap(t, root, request.Work.SessionID, team.ID, member.ID, turn.ID, shutdown.ID)
}

func assertRecoveredStoppedTerminalGap(t *testing.T, root, sessionID, teamID, memberID, turnID, requestID string) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn status=%s, want succeeded durable outcome", got)
	}
	if got := projection.Members[memberID].Status; got != teams.MemberStopped {
		t.Fatalf("member status=%s, want stopped after approved stop", got)
	}
	if got := projection.Requests[requestID].Status; got != teams.RequestApproved {
		t.Fatalf("shutdown request status=%s, want approved", got)
	}
	if got := projection.Teams[teamID].Status; got != teams.TeamOpen {
		t.Fatalf("team status=%s, want open (only the member was stopped)", got)
	}
}
