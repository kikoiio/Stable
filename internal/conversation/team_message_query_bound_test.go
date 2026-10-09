package conversation

import (
	"context"
	"fmt"
	"testing"

	"stable/internal/teams"
)

// Query limits are an AC8 resource boundary at the service API, not only a
// helper invariant: an oversized request must never materialize an unbounded
// page from the replayed team projection.
func TestListTeamMessagesClampsPageSizeAndPreservesCursor(t *testing.T) {
	service, request, team := newTeamMessageFixture(t)
	addTeamMessageMember(t, service, request, team.ID, "member-b", "reviewer")

	const total = teams.MaxPageSize + 1
	for i := 0; i < total; i++ {
		recipient := "member-a"
		if i%2 == 1 {
			recipient = "member-b"
		}
		if _, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
			TeamID: team.ID, Recipient: recipient, Body: fmt.Sprintf("query item %03d", i), Token: fmt.Sprintf("query-%03d", i),
		}); err != nil {
			t.Fatalf("persist message %d: %v", i, err)
		}
	}

	oversized, err := service.ListTeamMessages(context.Background(), request, team.ID, 0, total*10)
	if err != nil {
		t.Fatal(err)
	}
	if len(oversized) != teams.MaxPageSize {
		t.Fatalf("oversized query returned %d messages, want capped at %d", len(oversized), teams.MaxPageSize)
	}
	for i, message := range oversized {
		if message.Seq == 0 || i > 0 && oversized[i-1].Seq >= message.Seq {
			t.Fatalf("oversized query is not strictly ordered at %d: %+v", i, oversized)
		}
	}

	defaultPage, err := service.ListTeamMessages(context.Background(), request, team.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage) != teams.DefaultPageSize {
		t.Fatalf("zero limit returned %d messages, want default page %d", len(defaultPage), teams.DefaultPageSize)
	}

	negativePage, err := service.ListTeamMessages(context.Background(), request, team.ID, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(negativePage) != teams.DefaultPageSize {
		t.Fatalf("negative limit returned %d messages, want default page %d", len(negativePage), teams.DefaultPageSize)
	}

	after := oversized[len(oversized)-2].Seq
	continued, err := service.ListTeamMessages(context.Background(), request, team.ID, after, total*10)
	if err != nil {
		t.Fatal(err)
	}
	if len(continued) != 2 || continued[0].Seq <= after || continued[1].Seq <= continued[0].Seq {
		t.Fatalf("cursor query did not return the bounded remaining suffix after %d: %+v", after, continued)
	}
}
