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

type gatedExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (e gatedExecutor) Execute(context.Context, llm.ToolUse) (ToolOutcome, error) {
	e.started <- struct{}{}
	<-e.release
	return ToolOutcome{Status: ToolSucceeded, Content: "ok"}, nil
}

func TestRunnerPublishesDelegationEventsInRunSequence(t *testing.T) {
	var calls atomic.Int32
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		if calls.Add(1) == 1 {
			events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"a"}`), Complete: true}}
		} else {
			events <- llm.Event{Kind: llm.TextDelta, Text: "done"}
		}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		close(events)
		errs := make(chan error)
		close(errs)
		return events, errs
	})
	executor := gatedExecutor{started: make(chan struct{}, 1), release: make(chan struct{})}
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ExecutorFactory: FakeExecutorFactory{Executor: &FakeExecutor{Script: []ToolOutcome{{Status: ToolSucceeded}}}}})
	// Swap in the blocking executor through a small factory so the parent run
	// remains in the tool execution phase while collaboration is injected.
	runner.SetTooling(singleExecutorFactory{executor: executor}, nil)
	handle, err := runner.Start(context.Background(), sessionRequest("delegation-seq"))
	if err != nil {
		t.Fatal(err)
	}
	<-executor.started
	if err = runner.PublishDelegation("delegation-seq", DelegationEvent{BatchID: "batch", TaskID: "task", TaskName: "inspect", Status: DelegationRunning}); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	var seqs []uint64
	found := false
	for event := range handle.Events {
		seqs = append(seqs, event.RunSeq)
		if event.Kind == EventDelegation {
			found = true
		}
	}
	if outcome := <-handle.Done; outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if !found || len(seqs) == 0 {
		t.Fatalf("delegation event missing: seqs=%v", seqs)
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("run sequence=%v", seqs)
		}
	}
}

type singleExecutorFactory struct{ executor RunExecutor }

func (f singleExecutorFactory) ForRun(ExecutionRequest) (RunExecutor, error) { return f.executor, nil }

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

func TestRunnerUsesTrustedPerRunToolSchemaOverride(t *testing.T) {
	var tools []llm.ToolSchema
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		tools = append([]llm.ToolSchema(nil), request.Tools...)
		events := make(chan llm.Event, 1)
		errs := make(chan error)
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ToolSchemas: []llm.ToolSchema{{Name: "write_file"}, {Name: "read_file"}}})
	request := sessionRequest("coordinator-tools")
	request.ToolSchemas = []llm.ToolSchema{{Name: "team_send"}, {Name: "team_task_update"}}
	handle, err := runner.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events {
	}
	if outcome := <-handle.Done; outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(tools) != 2 || tools[0].Name != "team_send" || tools[1].Name != "team_task_update" {
		t.Fatalf("per-run tool schemas were not applied: %+v", tools)
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

func TestRunnerToolLoopConvergesAcrossTwoRounds(t *testing.T) {
	var requests []llm.Request
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		requests = append(requests, request)
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		if len(requests) == 1 {
			call := &llm.ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"README"}`), Complete: true}
			events <- llm.Event{Kind: llm.ToolCallStart, Tool: call}
			events <- llm.Event{Kind: llm.StreamEnd, StopReason: "tool_use"}
		} else {
			events <- llm.Event{Kind: llm.TextDelta, Text: "done"}
			events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
		}
		close(events)
		close(errs)
		return events, errs
	})
	executor := &FakeExecutor{Script: []ToolOutcome{{Content: "file contents"}}}
	runner := NewRunner(provider, RunnerOptions{
		MaxRetries:      -1,
		ExecutorFactory: FakeExecutorFactory{Executor: executor},
		ToolSchemas:     []llm.ToolSchema{{Name: "read"}},
	})
	handle, err := runner.Start(context.Background(), sessionRequest("run-two-rounds"))
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events {
	}
	if outcome := <-handle.Done; outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(requests) != 2 {
		t.Fatalf("provider requests=%d want 2", len(requests))
	}
	if got := requests[0].Tools; len(got) != 1 || got[0].Name != "read" {
		t.Fatalf("first request tools=%+v", got)
	}
	if len(requests[1].Messages) != 3 {
		t.Fatalf("second request messages=%+v", requests[1].Messages)
	}
	assistant := requests[1].Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolUses) != 1 || assistant.ToolUses[0].ID != "call-1" {
		t.Fatalf("assistant tool message=%+v", assistant)
	}
	results := requests[1].Messages[2]
	if results.Role != "user" || len(results.ToolResults) != 1 || results.ToolResults[0].ToolUseID != "call-1" || results.ToolResults[0].Content != "file contents" {
		t.Fatalf("tool result message=%+v", results)
	}
}

func TestRunnerExecutesToolCallsSeriallyAndPublishesPairs(t *testing.T) {
	var requests []llm.Request
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		requests = append(requests, request)
		events := make(chan llm.Event, 5)
		errs := make(chan error)
		if len(requests) > 1 {
			events <- llm.Event{Kind: llm.TextDelta, Text: "done"}
			events <- llm.Event{Kind: llm.StreamEnd, StopReason: "completed"}
			close(events)
			close(errs)
			return events, errs
		}
		for _, call := range []*llm.ToolCall{
			{ID: "call-a", Name: "first", Arguments: json.RawMessage(`{}`), Complete: true},
			{ID: "call-b", Name: "second", Arguments: json.RawMessage(`{}`), Complete: true},
		} {
			events <- llm.Event{Kind: llm.ToolCallStart, Tool: call}
		}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "tool_use"}
		close(events)
		close(errs)
		return events, errs
	})
	executor := &FakeExecutor{Script: []ToolOutcome{{Content: "one"}, {Content: "two"}}}
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ExecutorFactory: FakeExecutorFactory{Executor: executor}})
	handle, err := runner.Start(context.Background(), sessionRequest("run-serial"))
	if err != nil {
		t.Fatal(err)
	}
	var got []ExecutionEvent
	for event := range handle.Events {
		got = append(got, event)
	}
	if outcome := <-handle.Done; outcome.Status != RunCompleted {
		t.Fatalf("outcome=%+v", outcome)
	}
	calls := executor.Calls()
	if len(calls) != 2 || calls[0].ID != "call-a" || calls[1].ID != "call-b" {
		t.Fatalf("executor calls=%+v", calls)
	}
	if len(requests) != 2 || len(requests[1].Messages) != 3 {
		t.Fatalf("provider requests=%+v", requests)
	}
	results := requests[1].Messages[2]
	if results.Role != "user" || len(results.ToolResults) != 2 || results.ToolResults[0].ToolUseID != "call-a" || results.ToolResults[0].Content != "one" || results.ToolResults[1].ToolUseID != "call-b" || results.ToolResults[1].Content != "two" {
		t.Fatalf("tool result message=%+v", results)
	}
	var toolEvents []EventKind
	for _, event := range got {
		if event.Kind == EventToolExecStart || event.Kind == EventToolExecResult {
			toolEvents = append(toolEvents, event.Kind)
		}
	}
	want := []EventKind{EventToolExecStart, EventToolExecResult, EventToolExecStart, EventToolExecResult}
	if len(toolEvents) != len(want) {
		t.Fatalf("tool events=%v want %v", toolEvents, want)
	}
	for i := range want {
		if toolEvents[i] != want[i] {
			t.Fatalf("tool events=%v want %v", toolEvents, want)
		}
	}
}

func TestRunnerStopsWhenToolRoundBudgetIsExhausted(t *testing.T) {
	var calls int
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		calls++
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		call := &llm.ToolCall{ID: "call-loop", Name: "loop", Arguments: json.RawMessage(`{}`), Complete: true}
		events <- llm.Event{Kind: llm.ToolCallStart, Tool: call}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "tool_use"}
		close(events)
		close(errs)
		return events, errs
	})
	executor := &FakeExecutor{Script: []ToolOutcome{{Content: "ok"}}}
	runner := NewRunner(provider, RunnerOptions{
		MaxRetries:      -1,
		ExecutorFactory: FakeExecutorFactory{Executor: executor},
		Budget:          ResourceBounds{MaxToolRounds: 1},
	})
	handle, err := runner.Start(context.Background(), sessionRequest("run-budget"))
	if err != nil {
		t.Fatal(err)
	}
	var budgetEvents int
	for event := range handle.Events {
		if event.Kind == EventBudgetExhausted {
			budgetEvents++
		}
	}
	if outcome := <-handle.Done; outcome.Status != RunBudgetExhausted {
		t.Fatalf("outcome=%+v", outcome)
	}
	if calls != 2 || len(executor.Calls()) != 1 || budgetEvents != 1 {
		t.Fatalf("provider calls=%d executor calls=%d budget events=%d", calls, len(executor.Calls()), budgetEvents)
	}
}

func TestRunnerCancellationStopsToolExecution(t *testing.T) {
	started := make(chan struct{})
	executor := &blockingExecutor{started: started}
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		errs := make(chan error)
		call := &llm.ToolCall{ID: "call-cancel", Name: "wait", Arguments: json.RawMessage(`{}`), Complete: true}
		events <- llm.Event{Kind: llm.ToolCallStart, Tool: call}
		events <- llm.Event{Kind: llm.StreamEnd, StopReason: "tool_use"}
		close(events)
		close(errs)
		return events, errs
	})
	runner := NewRunner(provider, RunnerOptions{MaxRetries: -1, ExecutorFactory: executorFactoryFunc(func(ExecutionRequest) (RunExecutor, error) { return executor, nil })})
	handle, err := runner.Start(context.Background(), sessionRequest("run-tool-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := runner.Cancel("run-tool-cancel"); err != nil {
		t.Fatal(err)
	}
	for range handle.Events {
	}
	if outcome := <-handle.Done; outcome.Status != RunCancelled {
		t.Fatalf("outcome=%+v", outcome)
	}
}

type executorFactoryFunc func(ExecutionRequest) (RunExecutor, error)

func (f executorFactoryFunc) ForRun(request ExecutionRequest) (RunExecutor, error) { return f(request) }

type blockingExecutor struct{ started chan<- struct{} }

func (e *blockingExecutor) Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error) {
	close(e.started)
	<-ctx.Done()
	return ToolOutcome{}, ctx.Err()
}
