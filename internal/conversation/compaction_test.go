package conversation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

// consumeRun turns a runner-issued compaction boundary into the run-scope
// session log boundary in stream order, keeping conversation the only log
// appender.
func TestConsumeRunPersistsCompactionBoundary(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan agent.ExecutionEvent, 3)
	done := make(chan agent.RunOutcome, 1)
	started := time.Now().UTC()
	boundaryPayload, _ := json.Marshal(agent.ContextBoundary{RunID: "run-1", FromSeq: 1, ToSeq: 1, Summary: "较早内容摘要"})
	events <- agent.ExecutionEvent{ID: "evt-1", RunID: "run-1", SessionID: session.ID, RunSeq: 1, At: started, Kind: agent.EventTextDelta, Payload: json.RawMessage(`{"text":"hello"}`)}
	events <- agent.ExecutionEvent{ID: "evt-2", RunID: "run-1", SessionID: session.ID, RunSeq: 2, At: started.Add(time.Millisecond), Kind: agent.EventCompactionBoundary, Payload: boundaryPayload}
	events <- agent.ExecutionEvent{ID: "evt-3", RunID: "run-1", SessionID: session.ID, RunSeq: 3, At: started.Add(2 * time.Millisecond), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`)}
	close(events)
	done <- agent.RunOutcome{RunID: "run-1", Status: agent.RunCompleted}
	close(done)
	runner := &fixedRunner{handle: &agent.RunHandle{Events: events, Done: done}}
	updates := make(chan ServerMsg, 16)
	svc := &Service{deps: Deps{Runner: runner, ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{updates: {ch: updates}}}
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "compact", Model: "mock"}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-updates:
			if msg.Type == "run_outcome" {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for run outcome")
		}
	}
done:
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var boundary *sessionlog.Boundary
	runEventCount := 0
	for i, event := range transcript.Events {
		if event.Type == sessionlog.EventRunEvent {
			runEventCount++
		}
		if event.Type == sessionlog.EventBoundary {
			var b sessionlog.Boundary
			raw, _ := json.Marshal(event.Data)
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			boundary = &b
			// The boundary lands immediately after its run event in the log.
			if transcript.Events[i-1].Type != sessionlog.EventRunEvent {
				t.Fatalf("boundary not in stream order at seq %d", event.Seq)
			}
		}
	}
	if runEventCount != 3 {
		t.Fatalf("run events = %d, want 3", runEventCount)
	}
	if boundary == nil || boundary.EffectiveScope() != sessionlog.BoundaryScopeRun || boundary.RunID != "run-1" || boundary.Summary != "较早内容摘要" || boundary.FromSeq != 1 || boundary.ToSeq != 1 {
		t.Fatalf("persisted boundary = %+v", boundary)
	}
}

// sessionConversationMessages applies the latest run boundary: covered
// deltas are replaced by the summary and post-boundary deltas start a fresh
// assistant message.
func TestSessionConversationMessagesAppliesRunBoundary(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "r1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	appendDelta := func(id string, seq uint64, text string) {
		t.Helper()
		if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: "r1", SessionID: session.ID, RunSeq: seq, At: time.Now().UTC(), Kind: "text_delta", Payload: map[string]string{"text": text}}); err != nil {
			t.Fatal(err)
		}
	}
	appendDelta("e1", 1, "旧的")
	appendDelta("e2", 2, "内容")
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventBoundary, sessionlog.Boundary{FromSeq: 1, ToSeq: 2, Summary: "摘要", Scope: sessionlog.BoundaryScopeRun, RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	appendDelta("e3", 3, "新的")
	messages := sessionConversationMessages(root, session.ID)
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	if messages[0].Role != "assistant" || messages[0].Content != "Earlier conversation summary: 摘要" {
		t.Fatalf("summary message = %+v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "新的" {
		t.Fatalf("tail message = %+v", messages[1])
	}
}

// A subscriber reconnecting with a cursor receives the boundary run event
// exactly once; the session log boundary is not duplicated into the stream.
func TestSubscribeRunReplaysBoundaryByCursor(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "r1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: "e1", RunID: "r1", SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(), Kind: "text_delta", Payload: map[string]string{"text": "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: "e2", RunID: "r1", SessionID: session.ID, RunSeq: 2, At: time.Now().UTC(), Kind: string(agent.EventCompactionBoundary), Payload: map[string]any{"run_id": "r1", "from_seq": 1, "to_seq": 1, "summary": "s"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventBoundary, sessionlog.Boundary{FromSeq: 1, ToSeq: 2, Summary: "s", Scope: sessionlog.BoundaryScopeRun, RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{}}
	resume := make(chan ServerMsg, 8)
	svc.mu.Lock()
	svc.clients[resume] = &clientSubscription{ch: resume}
	svc.mu.Unlock()
	if err := svc.subscribeRun(context.Background(), ClientMsg{SessionID: session.ID, RunID: "r1"}, resume); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for range 2 {
		msg := <-resume
		if msg.RunEvent != nil {
			kinds = append(kinds, msg.RunEvent.Kind)
		}
	}
	if len(kinds) != 2 || kinds[0] != "text_delta" || kinds[1] != string(agent.EventCompactionBoundary) {
		t.Fatalf("replayed = %v", kinds)
	}
	select {
	case extra := <-resume:
		t.Fatalf("unexpected extra replay: %+v", extra)
	default:
	}
}
