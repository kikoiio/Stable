package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"stable/internal/llm"
)

// StreamingChildRunner executes one isolated child through the existing
// provider/tool loop. It does not persist or return the child's transcript.
type StreamingChildRunner struct{}

func (StreamingChildRunner) Run(ctx context.Context, input ChildRunInput) ChildRunResult {
	if input.Provider == nil || input.Model == "" || input.ExecutorFactory == nil || input.ChildRunID == "" || input.Task.Instruction == "" {
		return ChildRunResult{Status: DelegationFailed, Error: "child run is missing required execution context"}
	}
	resourceBounds, _ := json.Marshal(ResourceBounds{
		MaxToolRounds:    input.Budget.MaxToolRounds,
		MaxTotalDuration: input.Budget.MaxDuration,
	})
	request := ExecutionRequest{
		RunID:  input.ChildRunID,
		Work:   input.Work,
		Intent: input.Task.Instruction,
		Messages: []llm.Message{
			{Role: "system", Content: "You are a read-only research agent. Use only the available read, search, and directory listing tools. Do not attempt to modify files or use other capabilities. Return a concise factual summary with relevant paths."},
			{Role: "user", Content: input.Task.Instruction},
		},
		ProviderName:     input.ProviderName,
		Model:            input.Model,
		AllowedScope:     []string{input.ProjectRoot},
		PermissionBounds: append(json.RawMessage(nil), input.PermissionBounds...),
		ResourceBounds:   resourceBounds,
	}
	outputBudget := &childOutputBudget{remaining: input.Budget.MaxToolOutputBytes}
	runner := NewRunner(input.Provider, RunnerOptions{
		MaxRetries:      -1,
		ExecutorFactory: cappedExecutorFactory{inner: input.ExecutorFactory, budget: outputBudget},
		ToolSchemas:     append([]llm.ToolSchema(nil), input.ToolSchemas...),
		Budget:          ResourceBounds{MaxToolRounds: input.Budget.MaxToolRounds, MaxTotalDuration: input.Budget.MaxDuration},
	})
	handle, err := runner.Start(ctx, request)
	if err != nil {
		return ChildRunResult{Status: DelegationFailed, Error: "could not start child run"}
	}
	var summary strings.Builder
	summaryTruncated := false
	for event := range handle.Events {
		switch event.Kind {
		case EventTextDelta:
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				remaining := input.Budget.MaxSummaryBytes - summary.Len()
				if remaining > 0 {
					if len(payload.Text) > remaining {
						payload.Text = truncateUTF8(payload.Text, remaining)
						summaryTruncated = true
					}
					summary.WriteString(payload.Text)
				} else if payload.Text != "" {
					summaryTruncated = true
				}
			}
		case EventToolExecResult:
			var outcome ToolOutcome
			if json.Unmarshal(event.Payload, &outcome) == nil {
				stage := "tool_failed"
				if outcome.Status == ToolSucceeded && !outcome.IsError {
					stage = "tool_completed"
				}
				if input.Progress != nil {
					input.Progress(stage, fmt.Sprintf("%s: %s", outcome.ToolName, stage))
				}
			}
		case EventBudgetExhausted:
			if input.Progress != nil {
				input.Progress("budget_exhausted", "child resource limit reached")
			}
		}
	}
	outcome := <-handle.Done
	text := strings.TrimSpace(summary.String())
	if summaryTruncated {
		text = truncateUTF8(text, input.Budget.MaxSummaryBytes)
	}
	outputBudget.mu.Lock()
	outputLimitReached := outputBudget.exceeded
	outputBudget.mu.Unlock()
	if outputLimitReached {
		return ChildRunResult{Status: DelegationFailed, Summary: truncateUTF8(text, input.Budget.MaxSummaryBytes), Error: "child tool output limit reached"}
	}
	if text == "" && outcome.Status == RunCompleted {
		text = "Read-only investigation completed without a text summary."
	}
	switch outcome.Status {
	case RunCompleted:
		return ChildRunResult{Status: DelegationSucceeded, Summary: truncateUTF8(text, input.Budget.MaxSummaryBytes)}
	case RunCancelled:
		return ChildRunResult{Status: DelegationCanceled, Summary: truncateUTF8(text, input.Budget.MaxSummaryBytes), Error: "child run canceled"}
	case RunBudgetExhausted:
		return ChildRunResult{Status: DelegationFailed, Summary: truncateUTF8(text, input.Budget.MaxSummaryBytes), Error: "child resource budget exhausted"}
	default:
		reason := "child run failed"
		if outcome.Error != nil && outcome.Error.Message != "" {
			reason = outcome.Error.Message
		}
		return ChildRunResult{Status: DelegationFailed, Summary: truncateUTF8(text, input.Budget.MaxSummaryBytes), Error: reason}
	}
}

type cappedExecutorFactory struct {
	inner  ExecutorFactory
	budget *childOutputBudget
}

type childOutputBudget struct {
	mu        sync.Mutex
	remaining int
	exceeded  bool
}

func (f cappedExecutorFactory) ForRun(request ExecutionRequest) (RunExecutor, error) {
	inner, err := f.inner.ForRun(request)
	if err != nil {
		return nil, err
	}
	return &cappedExecutor{inner: inner, budget: f.budget}, nil
}

type cappedExecutor struct {
	inner  RunExecutor
	budget *childOutputBudget
}

func (e *cappedExecutor) Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error) {
	outcome, err := e.inner.Execute(ctx, call)
	if err != nil {
		return outcome, err
	}
	e.budget.mu.Lock()
	defer e.budget.mu.Unlock()
	used := outcome.OutputBytes
	if used <= 0 {
		used = len(outcome.Content)
	}
	if used > e.budget.remaining {
		outcome.Content = truncateBytesUTF8(outcome.Content, e.budget.remaining)
		outcome.OutputBytes = e.budget.remaining
		outcome.IsError = true
		outcome.Status = ToolFailed
		e.budget.exceeded = true
		e.budget.remaining = 0
		return outcome, nil
	}
	e.budget.remaining -= used
	return outcome, nil
}

func truncateBytesUTF8(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

var _ ChildRunner = StreamingChildRunner{}
