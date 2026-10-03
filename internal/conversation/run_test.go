package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
)

type fixedRunner struct {
	handle    *agent.RunHandle
	cancelled string
	request   agent.ExecutionRequest
}

func (r *fixedRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	r.request = request
	return r.handle, nil
}
func (r *fixedRunner) Cancel(runID string) error { r.cancelled = runID; return nil }

func TestStartRunPersistsBeforeBroadcastAndReplays(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan agent.ExecutionEvent, 2)
	done := make(chan agent.RunOutcome, 1)
	started := time.Now().UTC()
	events <- agent.ExecutionEvent{ID: "evt-text", RunID: "run-1", SessionID: session.ID, RunSeq: 1, At: started, Kind: agent.EventTextDelta, Payload: json.RawMessage(`{"text":"hello"}`)}
	events <- agent.ExecutionEvent{ID: "evt-end", RunID: "run-1", SessionID: session.ID, RunSeq: 2, At: started.Add(time.Millisecond), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`)}
	close(events)
	done <- agent.RunOutcome{RunID: "run-1", Status: agent.RunCompleted}
	close(done)
	runner := &fixedRunner{handle: &agent.RunHandle{Events: events, Done: done}}
	updates := make(chan ServerMsg, 8)
	svc := &Service{deps: Deps{Runner: runner, ProjectRoot: root, ProviderName: "openai-compatible", Model: "mock", ProviderCredential: "fixture-secret"}, clients: map[chan ServerMsg]*clientSubscription{updates: {ch: updates}}}
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "say fixture-secret", Messages: []llm.Message{{Role: "user", Content: "fixture-secret"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	var received []ServerMsg
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-updates:
			received = append(received, msg)
			if msg.Type == "run_outcome" {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for run outcome")
		}
	}
done:
	if len(received) != 4 || received[0].Type != "run_started" || received[1].Type != "run_event" || received[2].Type != "run_event" || received[3].Type != "run_outcome" {
		t.Fatalf("messages=%+v", received)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != 5 || transcript.Events[1].Type != sessionlog.EventMessage || transcript.Events[2].Type != sessionlog.EventRunStarted || transcript.Events[3].Seq != received[1].Cursor {
		t.Fatalf("persisted transcript=%+v", transcript.Events)
	}
	if len(runner.request.Messages) != 2 || runner.request.Messages[0].Role != "system" || runner.request.Messages[1].Content != "[credential redacted]" || runner.request.Intent != "say [credential redacted]" {
		t.Fatalf("runner request did not include system and user message: %+v", runner.request.Messages)
	}
	encoded, _ := json.Marshal(transcript.Events)
	if strings.Contains(string(encoded), "fixture-secret") {
		t.Fatalf("provider key leaked to transcript: %s", encoded)
	}
	resume := make(chan ServerMsg, 8)
	svc.mu.Lock()
	svc.clients[resume] = &clientSubscription{ch: resume}
	svc.mu.Unlock()
	if err := svc.subscribeRun(context.Background(), ClientMsg{SessionID: session.ID, RunID: "run-1", AfterSeq: 2}, resume); err != nil {
		t.Fatal(err)
	}
	first := <-resume
	if first.Type != "run_event" || first.RunEvent == nil || first.RunEvent.ID != "evt-text" {
		t.Fatalf("replay=%+v", first)
	}
	var sawOutcome bool
	for range 2 {
		message := <-resume
		if message.Type == "run_outcome" && message.Outcome != nil && message.Outcome.Status == agent.RunCompleted {
			sawOutcome = true
		}
	}
	if !sawOutcome {
		t.Fatal("terminal outcome was not recoverable from the replay")
	}
}

func TestRunCancelChecksSessionOwnership(t *testing.T) {
	runner := &fixedRunner{}
	svc := &Service{deps: Deps{Runner: runner}, activeRuns: map[string]string{"run-1": "session-1"}}
	if err := svc.cancelRun(ClientMsg{RunID: "run-1", SessionID: "other"}, make(chan ServerMsg, 1)); err == nil {
		t.Fatal("cross-session cancel accepted")
	}
	if err := svc.cancelRun(ClientMsg{RunID: "run-1", SessionID: "session-1"}, make(chan ServerMsg, 1)); err != nil {
		t.Fatal(err)
	}
	if runner.cancelled != "run-1" {
		t.Fatalf("cancelled=%q", runner.cancelled)
	}
	if err := svc.cancelRun(ClientMsg{RunID: "unknown", SessionID: "session-1"}, make(chan ServerMsg, 1)); err != nil {
		t.Fatalf("repeat/unknown cancel: %v", err)
	}
}

func TestBroadcastRunSignalsBackpressureWithCursor(t *testing.T) {
	ch := make(chan ServerMsg, 1)
	svc := &Service{clients: map[chan ServerMsg]*clientSubscription{ch: {ch: ch, sessionID: "s1"}}}
	svc.broadcastRun(ServerMsg{Type: "run_event"}, "s1", "r1", 5)
	if len(ch) != 1 {
		t.Fatal("first event was not queued")
	}
	svc.broadcastRun(ServerMsg{Type: "run_event"}, "s1", "r1", 6)
	if len(ch) != 1 {
		t.Fatal("overflow unexpectedly queued a second event")
	}
	<-ch
	svc.broadcastRun(ServerMsg{Type: "run_event"}, "s1", "r1", 7)
	resync := <-ch
	if resync.Type != "resync" || resync.Cursor != 5 {
		t.Fatalf("resync=%+v", resync)
	}
}

func TestSessionConversationMessagesRebuildsAssistantStream(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "r1", WorkKind: "session", Intent: "first"}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"hel", "lo"} {
		data := sessionlog.RunEvent{ID: fmt.Sprintf("e%d", i+1), RunID: "r1", SessionID: session.ID, RunSeq: uint64(i + 1), At: time.Now().UTC(), Kind: "text_delta", Payload: map[string]string{"text": text}}
		if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, data); err != nil {
			t.Fatal(err)
		}
	}
	messages := sessionConversationMessages(root, session.ID)
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" || messages[1].Content != "hello" {
		t.Fatalf("rebuilt history=%+v", messages)
	}
}
