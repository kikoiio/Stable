package conversation

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// Compaction is a parent-model-context boundary only: it must leave all
// durable team facts replayable, while a reconnect continues the parent run
// from its own session cursor and sees each remaining run event once.
func TestTeamFactsSurviveParentCompactionAndRunReconnect(t *testing.T) {
	root := t.TempDir()
	service, request := teamServiceFixture(t, root, "team-compaction-parent")
	team, err := service.CreateTeam(context.Background(), request, "compaction-replay")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, team.ID, "member-compaction", "reader")
	message, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: "member-compaction", Body: "Inspect the saved request and task.", Token: "compaction-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "inspect compaction boundary", Description: "Keep this task after the parent compacts."})
	if err != nil {
		t.Fatal(err)
	}
	shutdown, err := service.RequestTeamShutdown(context.Background(), request, team.ID, "member-compaction")
	if err != nil {
		t.Fatal(err)
	}
	if shutdown.Status != teams.RequestApproved {
		t.Fatalf("idle member shutdown request=%+v, want approved", shutdown)
	}
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Messages[message.ID].ID == "" || before.Tasks[task.ID].ID == "" || before.Requests[shutdown.ID].Status != teams.RequestApproved {
		t.Fatalf("fixture lacks durable team facts before compaction: %+v", before)
	}

	appendParentEvent := func(id string, seq uint64, kind agent.EventKind, payload any) sessionlog.Event {
		t.Helper()
		stored, appendErr := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: id, RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: seq,
			At: time.Now().UTC(), Kind: string(kind), Payload: payload,
		})
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return stored
	}
	first := appendParentEvent("parent-before-compact", 1, agent.EventTextDelta, map[string]string{"text": "covered parent text"})
	boundaryPayload, err := json.Marshal(agent.ContextBoundary{RunID: request.RunID, FromSeq: 1, ToSeq: 1, Summary: "parent summary"})
	if err != nil {
		t.Fatal(err)
	}
	boundaryRunEvent := appendParentEvent("parent-compaction-boundary", 2, agent.EventCompactionBoundary, json.RawMessage(boundaryPayload))
	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventBoundary, sessionlog.Boundary{
		FromSeq: 1, ToSeq: 1, Summary: "parent summary", Scope: sessionlog.BoundaryScopeRun, RunID: request.RunID,
	}); err != nil {
		t.Fatal(err)
	}
	postBoundary := appendParentEvent("parent-after-compact", 3, agent.EventTextDelta, map[string]string{"text": "post-compaction parent event"})

	after, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("parent compaction changed durable team projection:\nbefore=%+v\nafter=%+v", before, after)
	}

	updates := make(chan ServerMsg, 8)
	subscriber := &Service{deps: Deps{ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{updates: {ch: updates}}}
	if err := subscriber.subscribeRun(context.Background(), ClientMsg{
		SessionID: request.Work.SessionID, RunID: request.RunID, AfterSeq: first.Seq,
	}, updates); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id     string
		kind   string
		cursor uint64
	}{{"parent-compaction-boundary", string(agent.EventCompactionBoundary), boundaryRunEvent.Seq}, {"parent-after-compact", string(agent.EventTextDelta), postBoundary.Seq}}
	var previous uint64
	for i, expected := range want {
		select {
		case msg := <-updates:
			if msg.Type != "run_event" || msg.RunID != request.RunID || msg.RunEvent == nil || msg.RunEvent.ID != expected.id || msg.RunEvent.Kind != expected.kind || msg.Cursor != expected.cursor || msg.Cursor <= first.Seq || i > 0 && msg.Cursor <= previous {
				t.Fatalf("reconnected parent event %d = %+v, want %s at cursor %d", i, msg, expected.id, expected.cursor)
			}
			previous = msg.Cursor
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for parent run event %s", expected.id)
		}
	}
	select {
	case extra := <-updates:
		t.Fatalf("reconnect replay duplicated or added an event: %+v", extra)
	default:
	}
	final, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil || !reflect.DeepEqual(before, final) {
		t.Fatalf("parent reconnect changed team facts: err=%v projection-equal=%v", err, reflect.DeepEqual(before, final))
	}
}
