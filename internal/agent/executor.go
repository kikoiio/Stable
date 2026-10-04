package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"stable/internal/llm"
)

// ToolExecStatus describes the result of one tool call. Tool-level failures
// are represented by ToolOutcome rather than returned as Go errors.
type ToolExecStatus string

const (
	ToolSucceeded ToolExecStatus = "succeeded"
	ToolFailed    ToolExecStatus = "failed"
	ToolDenied    ToolExecStatus = "denied"
	ToolTimeout   ToolExecStatus = "timeout"
)

type DiffSummary struct {
	Additions int    `json:"additions"`
	Removals  int    `json:"removals"`
	Text      string `json:"text"`
}

type ToolOutcome struct {
	CallID      string         `json:"call_id"`
	ToolName    string         `json:"tool_name"`
	Content     string         `json:"content"`
	IsError     bool           `json:"is_error"`
	Status      ToolExecStatus `json:"status"`
	Elapsed     time.Duration  `json:"elapsed"`
	OutputBytes int            `json:"output_bytes"`
	Diff        *DiffSummary   `json:"diff,omitempty"`
}

type RunExecutor interface {
	Execute(context.Context, llm.ToolUse) (ToolOutcome, error)
}

type ExecutorFactory interface {
	ForRun(ExecutionRequest) (RunExecutor, error)
}

// ApprovalObserver is implemented by executors that may block waiting for a
// trusted user decision. The runner uses it to surface that state to clients.
type ApprovalObserver interface {
	SetApprovalObserver(func())
}

// FakeExecutor is a deterministic, context-aware executor for runner tests.
// It returns Script (or Outcomes, when Script is nil) in order. A scripted
// outcome is a tool-level result; Errors are reserved for executor failures.
type FakeExecutor struct {
	Script       []ToolOutcome
	Outcomes     []ToolOutcome
	Errors       []error
	Err          error
	Delay        time.Duration
	DelayPerCall time.Duration

	mu    sync.Mutex
	next  int
	calls []llm.ToolUse
}

func (f *FakeExecutor) Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error) {
	if f == nil {
		return ToolOutcome{}, errors.New("fake executor is nil")
	}
	start := time.Now()
	f.mu.Lock()
	index := f.next
	f.next++
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	delay := f.Delay
	if f.DelayPerCall != 0 {
		delay = f.DelayPerCall
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ToolOutcome{}, ctx.Err()
		case <-timer.C:
		}
	} else if err := ctx.Err(); err != nil {
		return ToolOutcome{}, err
	}

	if f.Err != nil {
		return ToolOutcome{}, f.Err
	}
	if index < len(f.Errors) && f.Errors[index] != nil {
		return ToolOutcome{}, f.Errors[index]
	}
	outcomes := f.Script
	if outcomes == nil {
		outcomes = f.Outcomes
	}
	if index >= len(outcomes) {
		return ToolOutcome{}, errors.New("fake executor script exhausted")
	}
	outcome := outcomes[index]
	if outcome.CallID == "" {
		outcome.CallID = call.ID
	}
	if outcome.ToolName == "" {
		outcome.ToolName = call.Name
	}
	if outcome.Status == "" {
		outcome.Status = ToolSucceeded
	}
	if outcome.IsError && outcome.Status == ToolSucceeded {
		outcome.Status = ToolFailed
	}
	if outcome.Elapsed == 0 {
		outcome.Elapsed = time.Since(start)
	}
	if outcome.OutputBytes == 0 {
		outcome.OutputBytes = len(outcome.Content)
	}
	return outcome, nil
}

// Calls returns a copy of calls received so far.
func (f *FakeExecutor) Calls() []llm.ToolUse {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.ToolUse(nil), f.calls...)
}

// FakeExecutorFactory binds one fake executor to every run.
type FakeExecutorFactory struct {
	Executor *FakeExecutor
	Err      error
}

func (f FakeExecutorFactory) ForRun(ExecutionRequest) (RunExecutor, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.Executor == nil {
		return nil, errors.New("fake executor is not configured")
	}
	return f.Executor, nil
}
