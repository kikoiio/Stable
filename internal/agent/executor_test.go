package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"stable/internal/llm"
)

func TestFakeExecutorReturnsScriptInOrder(t *testing.T) {
	fake := &FakeExecutor{Script: []ToolOutcome{
		{Content: "first"},
		{Content: "second", IsError: true, Status: ToolFailed},
	}}
	first := llm.ToolUse{ID: "call-1", Name: "read"}
	second := llm.ToolUse{ID: "call-2", Name: "write"}
	got, err := fake.Execute(context.Background(), first)
	if err != nil || got.Content != "first" || got.CallID != first.ID || got.ToolName != first.Name || got.Status != ToolSucceeded {
		t.Fatalf("first outcome=%+v err=%v", got, err)
	}
	got, err = fake.Execute(context.Background(), second)
	if err != nil || got.Content != "second" || got.CallID != second.ID || got.ToolName != second.Name || got.Status != ToolFailed || !got.IsError {
		t.Fatalf("second outcome=%+v err=%v", got, err)
	}
	if calls := fake.Calls(); len(calls) != 2 || calls[0].ID != first.ID || calls[1].ID != second.ID {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestFakeExecutorScriptExhaustionAndExecutorError(t *testing.T) {
	fake := &FakeExecutor{Script: []ToolOutcome{{Content: "only"}}}
	_, _ = fake.Execute(context.Background(), llm.ToolUse{ID: "call-1"})
	if _, err := fake.Execute(context.Background(), llm.ToolUse{ID: "call-2"}); err == nil {
		t.Fatal("expected script exhaustion error")
	}
	want := errors.New("executor failed")
	fake = &FakeExecutor{Err: want}
	if _, err := fake.Execute(context.Background(), llm.ToolUse{}); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}

func TestFakeExecutorHonorsDelayAndCancellation(t *testing.T) {
	fake := &FakeExecutor{Delay: time.Hour, Script: []ToolOutcome{{Content: "never"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fake.Execute(ctx, llm.ToolUse{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestFakeExecutorFactory(t *testing.T) {
	fake := &FakeExecutor{Script: []ToolOutcome{{Content: "ok"}}}
	executor, err := (FakeExecutorFactory{Executor: fake}).ForRun(ExecutionRequest{})
	if err != nil || executor != fake {
		t.Fatalf("executor=%v err=%v", executor, err)
	}
}
