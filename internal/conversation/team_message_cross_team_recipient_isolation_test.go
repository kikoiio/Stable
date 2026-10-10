package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMessageRecipientIDIsScopedWhenTeamsShareMemberName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "same-name-message-lead")
	teamA, err := service.CreateTeam(context.Background(), request, "parser-review-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(context.Background(), request, "parser-review-b")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, teamA.ID, "member-same-name-a", "reader")
	addTeamMessageMember(t, service, request, teamB.ID, "member-same-name-b", "reader")

	beforeA, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamA.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamB.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	// A valid ID from another team is not a valid recipient in team A, even
	// when both teams have a member with the same display name.
	if _, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: teamA.ID, Recipient: "member-same-name-b", Body: "cross-team message", Token: "foreign-member-id",
	}); err == nil {
		t.Fatal("team A accepted team B's member ID as a recipient")
	}
	afterA, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamA.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamB.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeA, afterA) || !reflect.DeepEqual(beforeB, afterB) {
		t.Fatalf("rejected cross-team recipient changed facts: teamA=%d/%d teamB=%d/%d", len(beforeA), len(afterA), len(beforeB), len(afterB))
	}

	// The same text remains ordinary content when sent to a local recipient;
	// it cannot create a shutdown request or change either member's state.
	message, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: teamA.ID, Recipient: "reader", Body: "[shutdown] plain team message", Token: "local-shutdown-text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if message.SenderID != teams.Lead || !reflect.DeepEqual(message.Recipients, []string{"member-same-name-a"}) {
		t.Fatalf("message identity/recipient escaped team A scope: %+v", message)
	}
	projectionA, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	projectionB, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectionA.Requests) != 0 || len(projectionB.Requests) != 0 || projectionA.Members["member-same-name-a"].Status != teams.MemberCreated || projectionB.Members["member-same-name-b"].Status != teams.MemberCreated {
		t.Fatalf("ordinary shutdown text changed control state: A requests=%d member=%s; B requests=%d member=%s", len(projectionA.Requests), projectionA.Members["member-same-name-a"].Status, len(projectionB.Requests), projectionB.Members["member-same-name-b"].Status)
	}
}
