package conversation

import (
	"os"
	"path/filepath"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A crash can land after an unaccepted turn intent was durably aborted but
// before its newly created member was marked interrupted. Startup recovery
// must repair the member without starting a child or changing the aborted turn.
func TestTeamRecoveryRepairsMemberAfterAbortedIntentGapIdempotently(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "aborted-member-gap-parent")
	team, err := service.CreateTeam(t.Context(), request, "aborted-member-gap")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-aborted-gap", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{
		ID: "turn-aborted-member-gap", MemberID: member.ID, RunID: "child-aborted-member-gap",
		TaskID: "task-aborted-member-gap", OriginRunID: request.RunID,
		OriginCallID: "spawn-aborted-member-gap", Status: "intent",
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate process death immediately after TeamTurnAborted persisted and
	// before recovery could append the associated member state.
	turn.Status = "aborted"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnAborted, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Turns[turn.ID].Status != "aborted" || before.Members[member.ID].Status != teams.MemberCreated {
		t.Fatalf("fixture did not preserve aborted-turn/member-state gap: turn=%s member=%s", before.Turns[turn.ID].Status, before.Members[member.ID].Status)
	}

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	after, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Turns[turn.ID].Status != "aborted" || after.Members[member.ID].Status != teams.MemberInterrupted {
		t.Fatalf("recovered state: turn=%s member=%s, want aborted/interrupted", after.Turns[turn.ID].Status, after.Members[member.ID].Status)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	firstRecoveryEventCount := len(transcript.Events)
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunStarted {
			var start sessionlog.RunStarted
			if decodeSessionData(event.Data, &start) == nil && start.RunID == turn.RunID {
				t.Fatalf("recovery started child run after aborted intent: %+v", start)
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
		t.Fatalf("second recovery appended events: %d -> %d", firstRecoveryEventCount, len(transcript.Events))
	}
	final, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Turns[turn.ID].Status != "aborted" || final.Members[member.ID].Status != teams.MemberInterrupted {
		t.Fatalf("second recovery changed state: turn=%s member=%s", final.Turns[turn.ID].Status, final.Members[member.ID].Status)
	}
}
