package conversation

import (
	"os"
	"path/filepath"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestRecoveryStopsCapacityWaiterWhenTeamClosingAndIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "closing-capacity-waiter-parent")
	team, err := service.CreateTeam(t.Context(), request, "closing-capacity-waiter")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-closing-capacity-waiter", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	member.Status = teams.MemberWaitingCapacity
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberState, ActorID: "service", Member: &member,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the closing intent is durable but before the
	// capacity waiter is transitioned to stopped.
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	team.Status = teams.TeamClosing
	team.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamClosing, ActorID: "service", Team: &team,
	}); err != nil {
		t.Fatal(err)
	}

	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("recover closing capacity waiter: %v", err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberStopped {
		t.Fatalf("recovered capacity waiter status=%s, want stopped", got)
	}
	if got := projection.Teams[team.ID].Status; got != teams.TeamClosed {
		t.Fatalf("recovered team status=%s, want closed", got)
	}
	if len(projection.Turns) != 0 {
		t.Fatalf("capacity recovery fabricated a child turn: %+v", projection.Turns)
	}

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
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
