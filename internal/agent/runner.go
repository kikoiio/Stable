package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"stable/internal/llm"
)

const (
	defaultMaxRetries = 2
	maxRetryWait      = 2 * time.Second
	maxTotalRetryWait = 4 * time.Second
)

type RunnerOptions struct {
	MaxRetries int
	Sleep      func(context.Context, time.Duration) error
}

type runEntry struct{ cancel context.CancelFunc }

type StreamingRunner struct {
	provider llm.Provider
	options  RunnerOptions
	mu       sync.Mutex
	runs     map[string]runEntry
}

func NewRunner(provider llm.Provider, options RunnerOptions) *StreamingRunner {
	if options.MaxRetries < 0 {
		options.MaxRetries = 0
	} else if options.MaxRetries == 0 {
		options.MaxRetries = defaultMaxRetries
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	return &StreamingRunner{provider: provider, options: options, runs: map[string]runEntry{}}
}

func (r *StreamingRunner) Start(parent context.Context, request ExecutionRequest) (*RunHandle, error) {
	if r.provider == nil {
		return nil, errors.New("streaming provider is not configured")
	}
	if err := ValidateRequest(request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	if _, exists := r.runs[request.RunID]; exists {
		r.mu.Unlock()
		cancel()
		return nil, errors.New("run ID is already active")
	}
	r.runs[request.RunID] = runEntry{cancel: cancel}
	r.mu.Unlock()
	events := make(chan ExecutionEvent, 128)
	done := make(chan RunOutcome, 1)
	go func() {
		outcome := r.execute(ctx, request, events)
		r.mu.Lock()
		delete(r.runs, request.RunID)
		r.mu.Unlock()
		cancel()
		done <- outcome
		close(done)
		close(events)
	}()
	return &RunHandle{Events: events, Done: done}, nil
}

func (r *StreamingRunner) Cancel(runID string) error {
	r.mu.Lock()
	entry, ok := r.runs[runID]
	r.mu.Unlock()
	if !ok {
		return nil // terminal or unknown runs are safe to cancel repeatedly
	}
	entry.cancel()
	return nil
}

func ValidateRequest(request ExecutionRequest) error {
	if request.RunID == "" || request.Work.SessionID == "" || request.Intent == "" {
		return errors.New("run requires run ID, session ID, and intent")
	}
	switch request.Work.Kind {
	case WorkSession:
		if request.Work.GoalID != "" || request.Work.WorkItemID != "" {
			return errors.New("session work cannot include goal IDs")
		}
	case WorkGoal:
		if request.Work.GoalID == "" || request.Work.WorkItemID == "" {
			return errors.New("goal work requires goal and work item IDs")
		}
	default:
		return errors.New("unknown work kind")
	}
	if request.Model == "" {
		return errors.New("run requires a model")
	}
	return nil
}

func (r *StreamingRunner) execute(ctx context.Context, request ExecutionRequest, output chan<- ExecutionEvent) RunOutcome {
	var seq uint64
	var emittedContent bool
	var hasToolCall bool
	var streamErr error
	for attempt := 0; attempt <= r.options.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return r.terminal(request, output, &seq, RunCancelled, nil)
		}
		events, errs := r.provider.Stream(ctx, llm.Request{Model: request.Model, Messages: request.Messages})
		streamErr = nil
		ended := false
		usageSeen := false
		for events != nil || errs != nil {
			select {
			case <-ctx.Done():
				events, errs = nil, nil
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				if event.Kind == llm.StreamEnd {
					ended = true
					if !usageSeen {
						usage := event.Usage
						if usage == nil {
							usage = &llm.UsageInfo{}
						}
						payload, _ := json.Marshal(llm.Event{Kind: llm.Usage, Usage: usage})
						if err := r.publish(request, output, &seq, ExecutionEvent{Kind: EventUsage, Payload: payload}); err != nil {
							return r.terminal(request, output, &seq, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish usage event"})
						}
					}
					continue
				}
				if event.Kind == llm.Usage {
					usageSeen = true
				}
				if event.Kind == llm.TextDelta || event.Kind == llm.ThinkingDelta || event.Kind == llm.ToolCallStart || event.Kind == llm.ToolCallDelta || event.Kind == llm.ToolCallComplete {
					emittedContent = true
				}
				if event.Kind == llm.ToolCallStart || event.Kind == llm.ToolCallComplete {
					hasToolCall = true
				}
				mapped := mapLLMEvent(event)
				if mapped.Kind == "" {
					continue
				}
				if err := r.publish(request, output, &seq, mapped); err != nil {
					return r.terminal(request, output, &seq, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish run event"})
				}
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if err != nil {
					streamErr = sanitizeError(err)
				}
			}
		}
		if ctx.Err() != nil {
			return r.terminal(request, output, &seq, RunCancelled, nil)
		}
		if streamErr == nil && !ended {
			streamErr = &llm.ProviderError{Class: llm.ErrorProvider, Message: "provider stream ended without a terminal event"}
		}
		if streamErr == nil {
			status := RunCompleted
			if hasToolCall {
				status = RunAwaitingTools
			}
			return r.terminal(request, output, &seq, status, nil)
		}
		providerErr := sanitizeError(streamErr)
		if providerErr.Retryable && !emittedContent && attempt < r.options.MaxRetries {
			delay := retryDelay(attempt, providerErr.RetryAfter)
			if delay <= maxTotalRetryWait {
				payload, _ := json.Marshal(map[string]any{"attempt": attempt + 1, "delay_ms": delay.Milliseconds(), "class": providerErr.Class})
				if err := r.publish(request, output, &seq, ExecutionEvent{Kind: EventRetry, Payload: payload}); err != nil {
					return r.terminal(request, output, &seq, RunFailed, providerErr)
				}
				if err := r.options.Sleep(ctx, delay); err != nil {
					return r.terminal(request, output, &seq, RunCancelled, nil)
				}
				continue
			}
		}
		payload, _ := json.Marshal(providerErr)
		_ = r.publish(request, output, &seq, ExecutionEvent{Kind: EventError, Payload: payload})
		return r.terminal(request, output, &seq, RunFailed, providerErr)
	}
	return r.terminal(request, output, &seq, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "provider retries exhausted"})
}

func (r *StreamingRunner) publish(request ExecutionRequest, output chan<- ExecutionEvent, seq *uint64, event ExecutionEvent) error {
	*seq++
	id, err := newEventID()
	if err != nil {
		return err
	}
	event.ID = id
	event.RunID = request.RunID
	event.SessionID = request.Work.SessionID
	event.RunSeq = *seq
	event.At = time.Now().UTC()
	output <- event
	return nil
}

func (r *StreamingRunner) terminal(request ExecutionRequest, output chan<- ExecutionEvent, seq *uint64, status RunStatus, providerErr *llm.ProviderError) RunOutcome {
	payload, _ := json.Marshal(map[string]any{"status": status, "error": providerErr})
	_ = r.publish(request, output, seq, ExecutionEvent{Kind: EventTerminal, Payload: payload})
	return RunOutcome{RunID: request.RunID, Status: status, Error: providerErr}
}

func mapLLMEvent(event llm.Event) ExecutionEvent {
	kindMap := map[llm.EventKind]EventKind{
		llm.TextDelta: EventTextDelta, llm.ThinkingDelta: EventThinkingDelta,
		llm.ThinkingComplete: EventThinkingComplete, llm.ToolCallStart: EventToolCallStart,
		llm.ToolCallDelta: EventToolCallDelta, llm.ToolCallComplete: EventToolCallComplete,
		llm.Usage: EventUsage,
	}
	kind, ok := kindMap[event.Kind]
	if !ok {
		return ExecutionEvent{}
	}
	payload, _ := json.Marshal(event)
	return ExecutionEvent{Kind: kind, Payload: payload}
}

func sanitizeError(err error) *llm.ProviderError {
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr
	}
	return &llm.ProviderError{Class: llm.ErrorProvider, Message: "provider request failed"}
}

func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	delay := time.Duration(1<<attempt) * 250 * time.Millisecond
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > maxRetryWait {
		return maxRetryWait + time.Nanosecond
	}
	return delay
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create event id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
