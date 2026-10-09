package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMessagePendingAggregateQuotaIsAtomicAtServiceBoundary(t *testing.T) {
	service, request, team := newTeamMessageFixture(t)
	for i := 1; i < teams.MaxTeamMembers/2; i++ {
		addTeamMessageMember(t, service, request, team.ID, fmt.Sprintf("member-%d", i+1), fmt.Sprintf("reader-%d", i+1))
	}
	body := strings.Repeat("x", teams.MaxMessageBytes)
	recipients := []string{"member-a", "member-2", "member-3", "member-4"}
	for _, recipient := range recipients {
		for i := range teams.MaxRecipientPending {
			_, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
				TeamID: team.ID, Recipient: recipient, Body: body,
				Token: fmt.Sprintf("aggregate-%s-%02d", recipient, i),
			})
			if err != nil {
				t.Fatalf("fill pending quota for %s message %d: %v", recipient, i, err)
			}
		}
	}

	before, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(before.Messages); got != teams.MaxTeamPending {
		t.Fatalf("pending messages = %d, want exact team delivery boundary %d", got, teams.MaxTeamPending)
	}
	if got := len(before.Messages) * len(body); got != teams.MaxTeamPendingBytes {
		t.Fatalf("aggregate pending bytes = %d, want exact byte boundary %d", got, teams.MaxTeamPendingBytes)
	}

	rejected := TeamSendRequest{TeamID: team.ID, Recipient: recipients[0], Body: body, Token: "aggregate-quota-rejected-token"}
	for attempt := 1; attempt <= 2; attempt++ {
		message, sendErr := service.SendTeamMessage(context.Background(), request, rejected)
		if message.ID != "" || sendErr != teams.ErrCapacity {
			t.Fatalf("over-limit attempt %d = %+v, %v; want empty result and capacity error", attempt, message, sendErr)
		}
	}
	after, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages) || after.Teams[team.ID].Revision != before.Teams[team.ID].Revision {
		t.Fatalf("rejected sends changed durable projection: messages %d -> %d, revision %d -> %d", len(before.Messages), len(after.Messages), before.Teams[team.ID].Revision, after.Teams[team.ID].Revision)
	}
}

func TestTeamMessagePendingAggregateQuotaBroadcastRejectsAtomically(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "aggregate-broadcast-lead")
	team, err := service.CreateTeam(t.Context(), request, "aggregate-broadcast")
	if err != nil {
		t.Fatal(err)
	}
	for i := range teams.MaxTeamMembers {
		id := fmt.Sprintf("broadcast-%d", i)
		addTeamMessageMember(t, service, request, team.ID, id, fmt.Sprintf("reader-%d", i))
	}
	body := strings.Repeat("b", teams.MaxMessageBytes)
	// Each broadcast creates eight pending deliveries. Thirty-two messages
	// reach the 256-delivery / 2 MiB aggregate boundary exactly.
	for i := range teams.MaxTeamPending / teams.MaxTeamMembers {
		if _, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
			TeamID: team.ID, Broadcast: true, Body: body, Token: fmt.Sprintf("broadcast-fill-%02d", i),
		}); err != nil {
			t.Fatalf("fill broadcast %d: %v", i, err)
		}
	}
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(before.Messages) * teams.MaxTeamMembers; got != teams.MaxTeamPending || got*len(body) != teams.MaxTeamPendingBytes {
		t.Fatalf("broadcast boundary deliveries=%d bytes=%d, want %d/%d", got, got*len(body), teams.MaxTeamPending, teams.MaxTeamPendingBytes)
	}
	args := TeamSendRequest{TeamID: team.ID, Broadcast: true, Body: body, Token: "broadcast-quota-rejected-token"}
	for attempt := 1; attempt <= 2; attempt++ {
		message, sendErr := service.SendTeamMessage(context.Background(), request, args)
		if message.ID != "" || sendErr != teams.ErrCapacity {
			t.Fatalf("over-limit broadcast attempt %d = %+v, %v", attempt, message, sendErr)
		}
	}
	after, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages) || after.Teams[team.ID].Revision != before.Teams[team.ID].Revision {
		t.Fatalf("rejected broadcasts changed projection: messages %d -> %d, revision %d -> %d", len(before.Messages), len(after.Messages), before.Teams[team.ID].Revision, after.Teams[team.ID].Revision)
	}
}
