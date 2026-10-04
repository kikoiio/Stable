package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"stable/internal/llm"
)

type preparerFunc func(context.Context, string, []llm.Message, []uint64) (PreparedRun, error)

func (f preparerFunc) PrepareRun(ctx context.Context, runID string, messages []llm.Message, msgSeqs []uint64) (PreparedRun, error) {
	return f(ctx, runID, messages, msgSeqs)
}

func TestRunnerPublishesBoundaryBeforeCompactedRequest(t *testing.T) {
	var providerMessages []llm.Message
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		providerMessages = append([]llm.Message(nil), request.Messages...)
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		events <- llm.Event{Kind: llm.TextDelta, Text: "answer"}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		close(events)
		close(errs)
		return events, errs
	})
	history := []llm.Message{
		{Role: "user", Content: "old-1"},
		{Role: "assistant", Content: "old-2"},
		{Role: "user", Content: "recent"},
	}
	preparer := preparerFunc(func(_ context.Context, runID string, messages []llm.Message, msgSeqs []uint64) (PreparedRun, error) {
		return PreparedRun{
			Messages:  []llm.Message{{Role: "assistant", Content: "Earlier conversation summary: 摘要"}, messages[2]},
			HeadKept:  0,
			TailStart: 2,
			Boundary:  &ContextBoundary{RunID: runID, FromSeq: 1, ToSeq: 1, Summary: "摘要"},
		}, nil
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ContextManager: preparer})
	request := sessionRequest("run-compact")
	request.Messages = history
	handle, err := runner.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EventKind
	var boundary ContextBoundary
	for event := range handle.Events {
		kinds = append(kinds, event.Kind)
		if event.Kind == EventCompactionBoundary {
			if err := json.Unmarshal(event.Payload, &boundary); err != nil {
				t.Fatal(err)
			}
		}
	}
	outcome := <-handle.Done
	if outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(kinds) != 4 || kinds[0] != EventCompactionBoundary || kinds[1] != EventTextDelta || kinds[3] != EventTerminal {
		t.Fatalf("event order=%v", kinds)
	}
	if boundary.Summary != "摘要" || boundary.RunID != "run-compact" {
		t.Fatalf("boundary=%+v", boundary)
	}
	if len(providerMessages) != 2 || providerMessages[0].Content != "Earlier conversation summary: 摘要" || providerMessages[1].Content != "recent" {
		t.Fatalf("provider received=%+v", providerMessages)
	}
}

func TestRunnerContextFailureSkipsProvider(t *testing.T) {
	calls := 0
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		calls++
		events := make(chan llm.Event)
		errs := make(chan error)
		close(events)
		close(errs)
		return events, errs
	})
	preparer := preparerFunc(func(context.Context, string, []llm.Message, []uint64) (PreparedRun, error) {
		return PreparedRun{}, errors.New("summary failed")
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ContextManager: preparer})
	handle, err := runner.Start(context.Background(), sessionRequest("run-fail"))
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for event := range handle.Events {
		if event.Kind == EventError {
			sawError = true
		}
	}
	outcome := <-handle.Done
	if outcome.Status != RunFailed || !sawError {
		t.Fatalf("outcome=%+v sawError=%t", outcome, sawError)
	}
	if calls != 0 {
		t.Fatal("provider received an unprocessed over-budget request")
	}
	if outcome.Error == nil || outcome.Error.Message == "" {
		t.Fatalf("terminal error lost the cause: %+v", outcome.Error)
	}
}

func TestRunnerWithoutContextManagerUnchanged(t *testing.T) {
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		events <- llm.Event{Kind: llm.TextDelta, Text: "ok"}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1})
	handle, err := runner.Start(context.Background(), sessionRequest("run-plain"))
	if err != nil {
		t.Fatal(err)
	}
	for event := range handle.Events {
		if event.Kind == EventCompactionBoundary {
			t.Fatal("boundary published without a context manager")
		}
	}
	if outcome := <-handle.Done; outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
}
