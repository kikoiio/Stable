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
	MaxRetries      int
	Sleep           func(context.Context, time.Duration) error
	ExecutorFactory ExecutorFactory
	ToolSchemas     []llm.ToolSchema
	Budget          ResourceBounds
	// ContextManager prepares provider messages before each request and may
	// publish a persistent compaction boundary. Nil disables compaction.
	ContextManager ContextPreparer
}

type runEventSink struct {
	mu      sync.Mutex
	request ExecutionRequest
	output  chan<- ExecutionEvent
	seq     uint64
	closed  bool
}

func (s *runEventSink) Publish(event ExecutionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("run event stream is closed")
	}
	s.seq++
	id, err := newEventID()
	if err != nil {
		return err
	}
	event.ID, event.RunID = id, s.request.RunID
	event.SessionID, event.RunSeq = s.request.Work.SessionID, s.seq
	event.At = time.Now().UTC()
	s.output <- event
	return nil
}

func (s *runEventSink) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *runEventSink) Sequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

type runEntry struct {
	cancel context.CancelFunc
	sink   *runEventSink
}

type StreamingRunner struct {
	provider llm.Provider
	options  RunnerOptions
	mu       sync.Mutex
	runs     map[string]runEntry
}

func (r *StreamingRunner) SetTooling(factory ExecutorFactory, schemas []llm.ToolSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.options.ExecutorFactory = factory
	r.options.ToolSchemas = append([]llm.ToolSchema(nil), schemas...)
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
	events := make(chan ExecutionEvent, 128)
	done := make(chan RunOutcome, 1)
	sink := &runEventSink{request: request, output: events}
	r.runs[request.RunID] = runEntry{cancel: cancel, sink: sink}
	r.mu.Unlock()
	go func() {
		outcome := r.execute(ctx, request, events, sink)
		r.mu.Lock()
		sink.close()
		delete(r.runs, request.RunID)
		r.mu.Unlock()
		cancel()
		done <- outcome
		close(done)
		close(events)
	}()
	return &RunHandle{Events: events, Done: done}, nil
}

// PublishDelegation injects a collaboration event into the active parent run
// stream so it shares sequence numbers and persistence with normal run events.
func (r *StreamingRunner) PublishDelegation(runID string, event DelegationEvent) error {
	r.mu.Lock()
	entry, ok := r.runs[runID]
	if !ok {
		r.mu.Unlock()
		return errors.New("parent run is not active")
	}
	payload, err := json.Marshal(event)
	if err == nil {
		err = entry.sink.Publish(ExecutionEvent{Kind: EventDelegation, Payload: payload})
	}
	r.mu.Unlock()
	return err
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

func (r *StreamingRunner) execute(ctx context.Context, request ExecutionRequest, output chan<- ExecutionEvent, sink *runEventSink) RunOutcome {
	messages := append([]llm.Message(nil), request.Messages...)
	msgSeqs := make([]uint64, len(messages)) // request history predates this run
	budget := r.runBudget(request)
	startedAt := time.Now()
	if budget.MaxTotalDuration > 0 {
		request.RunDeadline = startedAt.Add(budget.MaxTotalDuration)
	}
	toolRounds := 0
	var executor RunExecutor

	for {
		if ctx.Err() != nil {
			return r.terminal(request, output, sink, RunCancelled, nil)
		}
		if budget.MaxTotalDuration > 0 && time.Since(startedAt) >= budget.MaxTotalDuration {
			return r.budgetExhausted(request, output, sink, "max_total_duration")
		}

		if r.options.ContextManager != nil {
			prepared, err := r.options.ContextManager.PrepareRun(ctx, request.RunID, messages, msgSeqs)
			if err != nil {
				providerErr := &llm.ProviderError{Class: llm.ErrorProvider, Message: "context preparation failed: " + err.Error()}
				payload, _ := json.Marshal(providerErr)
				_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventError, Payload: payload})
				return r.terminal(request, output, sink, RunFailed, providerErr)
			}
			if prepared.Boundary != nil {
				payload, _ := json.Marshal(prepared.Boundary)
				if err := r.publish(request, output, sink, ExecutionEvent{Kind: EventCompactionBoundary, Payload: payload}); err != nil {
					providerErr := &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish compaction boundary"}
					return r.terminal(request, output, sink, RunFailed, providerErr)
				}
				// The synthetic summary message aligns with the boundary
				// event so a later boundary can cover it in turn.
				newSeqs := append(append([]uint64{}, msgSeqs[:prepared.HeadKept]...), sink.Sequence())
				msgSeqs = append(newSeqs, msgSeqs[prepared.TailStart:]...)
				messages = prepared.Messages
			}
		}

		var (
			streamErr error
			ended     bool
		)
		var responseText string
		var toolCalls []llm.ToolUse
		pendingCalls := map[string]llm.ToolUse{}
		completedCalls := map[string]bool{}

		for attempt := 0; attempt <= r.options.MaxRetries; attempt++ {
			if ctx.Err() != nil {
				return r.terminal(request, output, sink, RunCancelled, nil)
			}
			streamErr = nil
			ended = false
			responseText = ""
			toolCalls = nil
			pendingCalls = map[string]llm.ToolUse{}
			completedCalls = map[string]bool{}
			emittedContent := false
			usageSeen := false
			toolSchemas := r.options.ToolSchemas
			if request.ToolSchemas != nil {
				toolSchemas = request.ToolSchemas
			}
			events, errs := r.provider.Stream(ctx, llm.Request{Model: request.Model, Messages: messages, Tools: toolSchemas})
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
							if err := r.publish(request, output, sink, ExecutionEvent{Kind: EventUsage, Payload: payload}); err != nil {
								return r.terminal(request, output, sink, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish usage event"})
							}
						}
						continue
					}
					if event.Kind == llm.Usage {
						usageSeen = true
					}
					switch event.Kind {
					case llm.TextDelta:
						responseText += event.Text
						emittedContent = true
					case llm.ThinkingDelta:
						emittedContent = true
					case llm.ToolCallStart, llm.ToolCallDelta, llm.ToolCallComplete:
						emittedContent = true
						if event.Tool != nil && event.Tool.ID != "" {
							call := llm.ToolUse{ID: event.Tool.ID, Name: event.Tool.Name, Arguments: append(json.RawMessage(nil), event.Tool.Arguments...)}
							pendingCalls[call.ID] = call
							if event.Kind == llm.ToolCallComplete || event.Tool.Complete {
								if !completedCalls[call.ID] {
									toolCalls = append(toolCalls, call)
									completedCalls[call.ID] = true
								}
							}
						}
					}
					mapped := mapLLMEvent(event)
					if mapped.Kind == "" {
						continue
					}
					if err := r.publish(request, output, sink, mapped); err != nil {
						return r.terminal(request, output, sink, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish run event"})
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
				return r.terminal(request, output, sink, RunCancelled, nil)
			}
			if streamErr == nil && !ended {
				streamErr = &llm.ProviderError{Class: llm.ErrorProvider, Message: "provider stream ended without a terminal event"}
			}
			if streamErr == nil {
				break
			}
			providerErr := sanitizeError(streamErr)
			if providerErr.Retryable && !emittedContent && attempt < r.options.MaxRetries {
				delay := retryDelay(attempt, providerErr.RetryAfter)
				if delay <= maxTotalRetryWait {
					payload, _ := json.Marshal(map[string]any{"attempt": attempt + 1, "delay_ms": delay.Milliseconds(), "class": providerErr.Class})
					if err := r.publish(request, output, sink, ExecutionEvent{Kind: EventRetry, Payload: payload}); err != nil {
						return r.terminal(request, output, sink, RunFailed, providerErr)
					}
					if err := r.options.Sleep(ctx, delay); err != nil {
						return r.terminal(request, output, sink, RunCancelled, nil)
					}
					continue
				}
			}
			payload, _ := json.Marshal(providerErr)
			_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventError, Payload: payload})
			return r.terminal(request, output, sink, RunFailed, providerErr)
		}

		assistant := llm.Message{Role: "assistant", Content: responseText}
		if len(toolCalls) == 0 {
			messages = append(messages, assistant)
			msgSeqs = append(msgSeqs, sink.Sequence())
			return r.terminal(request, output, sink, RunCompleted, nil)
		}
		assistant.ToolUses = toolCalls
		messages = append(messages, assistant)
		msgSeqs = append(msgSeqs, sink.Sequence())
		toolRounds++
		if budget.MaxToolRounds > 0 && toolRounds > budget.MaxToolRounds {
			return r.budgetExhausted(request, output, sink, "max_tool_rounds")
		}
		if budget.MaxTotalDuration > 0 && time.Since(startedAt) >= budget.MaxTotalDuration {
			return r.budgetExhausted(request, output, sink, "max_total_duration")
		}
		if r.options.ExecutorFactory == nil {
			return r.terminal(request, output, sink, RunAwaitingTools, nil)
		}
		if executor == nil {
			var err error
			executor, err = r.options.ExecutorFactory.ForRun(request)
			if err != nil || executor == nil {
				providerErr := &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not initialize tool executor"}
				payload, _ := json.Marshal(providerErr)
				_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventError, Payload: payload})
				return r.terminal(request, output, sink, RunFailed, providerErr)
			}
		}
		if observer, ok := executor.(ApprovalObserver); ok {
			observer.SetApprovalObserver(func() {
				payload, _ := json.Marshal(map[string]any{"status": "awaiting_approval"})
				_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventAwaitingApproval, Payload: payload})
			})
		}
		results := make([]llm.ToolResultPart, 0, len(toolCalls))
		for _, call := range toolCalls {
			if ctx.Err() != nil {
				return r.terminal(request, output, sink, RunCancelled, nil)
			}
			startPayload, _ := json.Marshal(map[string]any{"call_id": call.ID, "tool_name": call.Name, "seq": sink.Sequence() + 1})
			if err := r.publish(request, output, sink, ExecutionEvent{Kind: EventToolExecStart, Payload: startPayload}); err != nil {
				return r.terminal(request, output, sink, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish tool start event"})
			}
			outcome, err := executor.Execute(ctx, call)
			if err != nil {
				if ctx.Err() != nil {
					return r.terminal(request, output, sink, RunCancelled, nil)
				}
				providerErr := &llm.ProviderError{Class: llm.ErrorProvider, Message: "tool executor failed: " + err.Error()}
				payload, _ := json.Marshal(providerErr)
				_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventError, Payload: payload})
				return r.terminal(request, output, sink, RunFailed, providerErr)
			}
			resultPayload, _ := json.Marshal(outcome)
			if err := r.publish(request, output, sink, ExecutionEvent{Kind: EventToolExecResult, Payload: resultPayload}); err != nil {
				return r.terminal(request, output, sink, RunFailed, &llm.ProviderError{Class: llm.ErrorProvider, Message: "could not publish tool result event"})
			}
			results = append(results, llm.ToolResultPart{ToolUseID: call.ID, Content: outcome.Content, IsError: outcome.IsError})
		}
		messages = append(messages, llm.Message{Role: "user", ToolResults: results})
		msgSeqs = append(msgSeqs, sink.Sequence())
	}
}

func (r *StreamingRunner) runBudget(request ExecutionRequest) ResourceBounds {
	return parseResourceBounds(request.ResourceBounds, r.options.Budget)
}

func (r *StreamingRunner) budgetExhausted(request ExecutionRequest, output chan<- ExecutionEvent, sink *runEventSink, reason string) RunOutcome {
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventBudgetExhausted, Payload: payload})
	return r.terminal(request, output, sink, RunBudgetExhausted, nil)
}

func (r *StreamingRunner) publish(_ ExecutionRequest, _ chan<- ExecutionEvent, sink *runEventSink, event ExecutionEvent) error {
	return sink.Publish(event)
}

func (r *StreamingRunner) terminal(request ExecutionRequest, output chan<- ExecutionEvent, sink *runEventSink, status RunStatus, providerErr *llm.ProviderError) RunOutcome {
	payload, _ := json.Marshal(map[string]any{"status": status, "error": providerErr})
	_ = r.publish(request, output, sink, ExecutionEvent{Kind: EventTerminal, Payload: payload})
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
