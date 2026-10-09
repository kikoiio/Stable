package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamLeadPendingMessageQuotaIsAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "lead-message-quota")
	team, err := service.CreateTeam(context.Background(), leadRequest, "lead-message-quota")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "lead-quota-member"
	addTeamMessageMember(t, service, leadRequest, team.ID, memberID, "reader")

	memberRequest := leadRequest
	memberRequest.RunID = "lead-message-quota-child"
	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: memberID, TurnID: turnID, MemberName: "reader"}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberID, RunID: memberRequest.RunID, TaskID: turnID, OriginRunID: leadRequest.RunID, Status: "intent"}
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: memberRequest.RunID, WorkKind: string(leadRequest.Work.Kind), Intent: "report to lead",
		TeamID: team.ID, TeamMemberID: memberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}

	for i := range teams.MaxRecipientPending {
		if _, err := service.SendTeamMessage(context.Background(), memberRequest, TeamSendRequest{
			TeamID: team.ID, Recipient: teams.Lead, Body: "note", Token: fmt.Sprintf("lead-pending-%02d", i),
		}); err != nil {
			t.Fatalf("lead notification %d rejected before per-recipient boundary: %v", i+1, err)
		}
	}
	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(before.Messages); got != teams.MaxRecipientPending {
		t.Fatalf("pending lead messages=%d, want exact recipient quota %d", got, teams.MaxRecipientPending)
	}

	rejected := TeamSendRequest{TeamID: team.ID, Recipient: teams.Lead, Body: "note", Token: "lead-pending-over-limit"}
	for attempt := 1; attempt <= 2; attempt++ {
		message, sendErr := service.SendTeamMessage(context.Background(), memberRequest, rejected)
		if message.ID != "" || sendErr != teams.ErrCapacity {
			t.Fatalf("over-limit attempt %d = %+v, %v; want empty result and capacity error", attempt, message, sendErr)
		}
	}
	after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages) || after.Teams[team.ID].Revision != before.Teams[team.ID].Revision {
		t.Fatalf("rejected lead messages changed durable projection: messages %d -> %d, revision %d -> %d", len(before.Messages), len(after.Messages), before.Teams[team.ID].Revision, after.Teams[team.ID].Revision)
	}
}
