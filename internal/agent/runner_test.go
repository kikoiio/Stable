package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/llm"
)

type providerFunc func(context.Context, llm.Request) (<-chan llm.Event, <-chan error)

func (f providerFunc) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	return f(ctx, request)
}

func TestRunnerAssignsOrderedEventsAndCompletes(t *testing.T) {
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		events <- llm.Event{Kind: llm.TextDelta, Text: "answer"}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1})
	handle, err := runner.Start(context.Background(), sessionRequest("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	var events []ExecutionEvent
	for event := range handle.Events {
		events = append(events, event)
	}
	outcome := <-handle.Done
	if outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(events) != 3 || events[0].Kind != EventTextDelta || events[1].Kind != EventUsage || events[2].Kind != EventTerminal {
		t.Fatalf("events=%+v", events)
	}
	for i, event := range events {
		if event.RunSeq != uint64(i+1) || event.RunID != "run-1" || event.SessionID != "session-1" || event.ID == "" || event.At.IsZero() {
			t.Fatalf("event metadata=%+v", event)
		}
	}
}

func TestRunnerRetriesBeforeTextButPreservesTextOnFailure(t *testing.T) {
	var calls atomic.Int32
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		call := calls.Add(1)
		events := make(chan llm.Event, 2)
		errs := make(chan error, 1)
		if call == 1 {
			errs <- &llm.ProviderError{Class: llm.ErrorRateLimit, Message: "limited", Retryable: true, RetryAfter: time.Second}
		} else {
			events <- llm.Event{Kind: llm.TextDelta, Text: "partial"}
			errs <- &llm.ProviderError{Class: llm.ErrorNetwork, Message: "network", Retryable: true}
		}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: 2, Sleep: func(context.Context, time.Duration) error { return nil }})
	handle, err := runner.Start(context.Background(), sessionRequest("run-retry"))
	if err != nil {
		t.Fatal(err)
	}
	var got []ExecutionEvent
	for event := range handle.Events {
		got = append(got, event)
	}
	outcome := <-handle.Done
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d want 2", calls.Load())
	}
	if outcome.Status != RunFailed {
		t.Fatalf("outcome=%+v", outcome)
	}
	var retry, partial, terminal bool
	for _, event := range got {
		switch event.Kind {
		case EventRetry:
			retry = true
		case EventTextDelta:
			var payload llm.Event
			_ = json.Unmarshal(event.Payload, &payload)
			partial = payload.Text == "partial"
		case EventTerminal:
			terminal = true
		}
	}
	if !retry || !partial || !terminal {
		t.Fatalf("events did not preserve retry/partial/terminal: %+v", got)
	}
}

func TestRunnerDoesNotRetryAfterTextDelta(t *testing.T) {
	var calls atomic.Int32
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		calls.Add(1)
		events := make(chan llm.Event, 1)
		errs := make(chan error, 1)
		events <- llm.Event{Kind: llm.TextDelta, Text: "kept"}
		errs <- &llm.ProviderError{Class: llm.ErrorNetwork, Message: "network", Retryable: true}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: 2})
	handle, err := runner.Start(context.Background(), sessionRequest("run-partial"))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range handle.Events {
		if event.Kind == EventTextDelta {
			var payload llm.Event
			_ = json.Unmarshal(event.Payload, &payload)
			text += payload.Text
		}
	}
	outcome := <-handle.Done
	if calls.Load() != 1 || text != "kept" || outcome.Status != RunFailed {
		t.Fatalf("calls=%d text=%q outcome=%+v", calls.Load(), text, outcome)
	}
}

func TestRunnerToolCallEndsAwaitingTools(t *testing.T) {
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		call := &llm.ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{}`), Complete: true}
		events <- llm.Event{Kind: llm.ToolCallStart, Tool: call}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "tool_use"}
		close(events)
		close(errs)
		return events, errs
	})
	handle, err := NewRunner(provider, RunnerOptions{MaxRetries: -1}).Start(context.Background(), sessionRequest("run-tool"))
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events {
	}
	if got := (<-handle.Done).Status; got != RunAwaitingTools {
		t.Fatalf("status=%s", got)
	}
}

func TestRunnerCancelIsIdempotentAndClosesProvider(t *testing.T) {
	started := make(chan struct{})
	providerCancelled := make(chan struct{})
	provider := providerFunc(func(ctx context.Context, _ llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event)
		errs := make(chan error)
		close(started)
		go func() { <-ctx.Done(); close(providerCancelled); close(events); close(errs) }()
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1})
	handle, err := runner.Start(context.Background(), sessionRequest("run-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := runner.Cancel("run-cancel"); err != nil {
		t.Fatal(err)
	}
	if err := runner.Cancel("run-cancel"); err != nil {
		t.Fatal(err)
	}
	var terminals int
	for event := range handle.Events {
		if event.Kind == EventTerminal {
			terminals++
		}
	}
	if got := (<-handle.Done).Status; got != RunCancelled {
		t.Fatalf("status=%s", got)
	}
	select {
	case <-providerCancelled:
	case <-time.After(time.Second):
		t.Fatal("provider context was not cancelled")
	}
	if terminals != 1 {
		t.Fatalf("terminal events=%d", terminals)
	}
}

func TestValidateRequestWorkOwnership(t *testing.T) {
	if err := ValidateRequest(sessionRequest("normal")); err != nil {
		t.Fatal(err)
	}
	goal := ExecutionRequest{RunID: "goal", Work: WorkRef{Kind: WorkGoal, SessionID: "s", GoalID: "g", WorkItemID: "w"}, Intent: "do", Model: "m"}
	if err := ValidateRequest(goal); err != nil {
		t.Fatal(err)
	}
	goal.Work.WorkItemID = ""
	if err := ValidateRequest(goal); err == nil {
		t.Fatal("goal without work item accepted")
	}
	if err := ValidateRequest(ExecutionRequest{RunID: "bad", Work: WorkRef{Kind: WorkSession, SessionID: "s", GoalID: "g"}, Intent: "do", Model: "m"}); err == nil {
		t.Fatal("session WorkRef with goal accepted")
	}
}

func sessionRequest(id string) ExecutionRequest {
	return ExecutionRequest{RunID: id, Work: WorkRef{Kind: WorkSession, SessionID: "session-1"}, Intent: "question", Messages: []llm.Message{{Role: "user", Content: "hello"}}, ProviderName: "openai-compatible", Model: "mock"}
}
