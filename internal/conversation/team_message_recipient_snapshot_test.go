package conversation

import (
	"context"
	"reflect"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamBroadcastRecipientSnapshotDoesNotExpandAfterJoin(t *testing.T) {
	service, request, team := newTeamMessageFixture(t)

	beforeJoin, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: team.ID, Broadcast: true, Body: "first broadcast", Token: "snapshot-before-join",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeJoin.Recipients, []string{"member-a"}) {
		t.Fatalf("first broadcast recipients = %v, want only existing member", beforeJoin.Recipients)
	}

	addTeamMessageMember(t, service, request, team.ID, "member-b", "reviewer")
	afterJoin, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: team.ID, Broadcast: true, Body: "second broadcast", Token: "snapshot-after-join",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterJoin.Recipients, []string{"member-a", "member-b"}) {
		t.Fatalf("second broadcast recipients = %v, want both current members", afterJoin.Recipients)
	}
	if beforeJoin.Seq == 0 || afterJoin.Seq <= beforeJoin.Seq {
		t.Fatalf("broadcast sequence is not strictly increasing: first=%d second=%d", beforeJoin.Seq, afterJoin.Seq)
	}

	// Rebuild from the durable session log so the assertions exercise the
	// stored recipient snapshots and append order, not in-memory service state.
	restarted := &Service{deps: service.deps, activeRuns: map[string]string{request.RunID: request.Work.SessionID}}
	messages, err := restarted.ListTeamMessages(context.Background(), request, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID != beforeJoin.ID || messages[1].ID != afterJoin.ID {
		t.Fatalf("replayed messages are not in append order: %+v", messages)
	}
	if !reflect.DeepEqual(messages[0].Recipients, []string{"member-a"}) || !reflect.DeepEqual(messages[1].Recipients, []string{"member-a", "member-b"}) {
		t.Fatalf("replay changed recipient snapshots: %+v", messages)
	}

	projection, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Messages[beforeJoin.ID]; !reflect.DeepEqual(got.Recipients, []string{"member-a"}) {
		t.Fatalf("later member was added to the earlier durable message: %+v", got)
	}
}
