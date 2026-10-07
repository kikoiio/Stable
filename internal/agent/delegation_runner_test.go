package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"stable/internal/llm"
)

type fixedOutcomeExecutor struct{ outcome ToolOutcome }

func (e fixedOutcomeExecutor) Execute(context.Context, llm.ToolUse) (ToolOutcome, error) {
	return e.outcome, nil
}

func TestCappedExecutorStopsAtAggregateOutputLimit(t *testing.T) {
	budget := &childOutputBudget{remaining: 5}
	executor := cappedExecutor{inner: fixedOutcomeExecutor{outcome: ToolOutcome{Content: "abcdefghij", OutputBytes: 10, Status: ToolSucceeded}}, budget: budget}
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "call", Name: "read_file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Content) > 5 || outcome.Status != ToolFailed || !outcome.IsError {
		t.Fatalf("output cap not enforced: %+v", outcome)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if !budget.exceeded || budget.remaining != 0 {
		t.Fatalf("budget state=%+v", budget)
	}
}

func TestTruncateUTF8RespectsByteCap(t *testing.T) {
	for _, size := range []int{1, 2, 3, 4, 8} {
		got := truncateUTF8("hello 世界", size)
		if len(got) > size {
			t.Fatalf("size %d produced %d bytes (%q)", size, len(got), got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("size %d produced invalid UTF-8: %q", size, got)
		}
	}
}

func TestChildBudgetDefaultDurationIsBounded(t *testing.T) {
	limits := DefaultDelegationLimits()
	if limits.MaxDuration != 3*time.Minute || limits.MaxToolRounds != 8 {
		t.Fatalf("unexpected child budget defaults: %+v", limits)
	}
}

type captureChildFactory struct{ exec RunExecutor }

func (f captureChildFactory) ForRun(ExecutionRequest) (RunExecutor, error) { return f.exec, nil }

type captureChildExecutor struct {
	mu    sync.Mutex
	calls []string
}

func (e *captureChildExecutor) Execute(_ context.Context, call llm.ToolUse) (ToolOutcome, error) {
	e.mu.Lock()
	e.calls = append(e.calls, call.Name)
	e.mu.Unlock()
	return ToolOutcome{CallID: call.ID, ToolName: call.Name, Content: "path: config.yaml", OutputBytes: 17, Status: ToolSucceeded}, nil
}

func TestStreamingChildRunnerUsesOnlyAssignedTaskAndReadTools(t *testing.T) {
	var mu sync.Mutex
	var requests []llm.Request
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		mu.Lock()
		requests = append(requests, request)
		call := len(requests)
		mu.Unlock()
		events := make(chan llm.Event, 2)
		if call == 1 {
			events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: "child-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"config.yaml"}`), Complete: true}}
		} else {
			events <- llm.Event{Kind: llm.TextDelta, Text: "The config is at config.yaml."}
		}
		events <- llm.Event{Kind: llm.StreamEnd}
		close(events)
		errs := make(chan error)
		close(errs)
		return events, errs
	})
	executor := &captureChildExecutor{}
	input := ChildRunInput{
		ParentRunID: "parent", ChildRunID: "child", Work: WorkRef{Kind: WorkSession, SessionID: "session"},
		Task:        DelegationTask{ID: "task-1", Name: "find config", Instruction: "Find the active config file."},
		ProjectRoot: "/authorized", PermissionBounds: json.RawMessage(`{"run_id":"child"}`),
		Provider: provider, ProviderName: "mock", Model: "mock-model", Budget: DefaultDelegationLimits(),
		ToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ExecutorFactory: captureChildFactory{exec: executor},
	}
	result := (StreamingChildRunner{}).Run(context.Background(), input)
	if result.Status != DelegationSucceeded || !strings.Contains(result.Summary, "config.yaml") {
		t.Fatalf("child result=%+v", result)
	}
	if len(requests) != 2 || requests[0].Model != "mock-model" || len(requests[0].Messages) != 2 || strings.Contains(requests[0].Messages[0].Content, "parent conversation") || requests[0].Messages[1].Content != input.Task.Instruction {
		t.Fatalf("unexpected child provider context: %+v", requests)
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "read_file" {
		t.Fatalf("unexpected child tool schema: %+v", requests[0].Tools)
	}
	if len(executor.calls) != 1 || executor.calls[0] != "read_file" {
		t.Fatalf("child tool calls=%v", executor.calls)
	}
}

func TestStreamingChildRunnerCapsSummaryWhileStreaming(t *testing.T) {
	provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
		events := make(chan llm.Event, 2)
		events <- llm.Event{Kind: llm.TextDelta, Text: strings.Repeat("summary ", 1000)}
		events <- llm.Event{Kind: llm.StreamEnd}
		close(events)
		errs := make(chan error)
		close(errs)
		return events, errs
	})
	input := ChildRunInput{
		ChildRunID: "child", Work: WorkRef{Kind: WorkSession, SessionID: "session"},
		Task:        DelegationTask{ID: "task", Name: "inspect", Instruction: "Inspect config."},
		ProjectRoot: "/authorized", Provider: provider, Model: "mock-model",
		Budget:          DelegationLimits{MaxToolRounds: 1, MaxDuration: time.Second, MaxSummaryBytes: 32},
		ExecutorFactory: captureChildFactory{exec: &captureChildExecutor{}},
	}
	result := (StreamingChildRunner{}).Run(context.Background(), input)
	if result.Status != DelegationSucceeded || len(result.Summary) > 32 || !utf8.ValidString(result.Summary) {
		t.Fatalf("summary was not safely capped: status=%s bytes=%d", result.Status, len(result.Summary))
	}
}
