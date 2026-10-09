package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"stable/internal/llm"
)

type productionOutputBoundaryExecutor struct{ outputBytes int }

func (e productionOutputBoundaryExecutor) Execute(_ context.Context, call llm.ToolUse) (ToolOutcome, error) {
	return ToolOutcome{
		CallID: call.ID, ToolName: call.Name, Content: strings.Repeat("x", e.outputBytes),
		OutputBytes: e.outputBytes, Status: ToolSucceeded,
	}, nil
}

func TestStreamingChildRunnerEnforcesProductionDefaultAggregateOutputLimit(t *testing.T) {
	const limit = 50_000
	defaults := DefaultDelegationLimits()
	if defaults.MaxToolOutputBytes != limit {
		t.Fatalf("production default output limit=%d, want %d", defaults.MaxToolOutputBytes, limit)
	}

	for _, tc := range []struct {
		name       string
		outputSize int
		wantStatus DelegationStatus
	}{
		{name: "exact default limit", outputSize: limit, wantStatus: DelegationSucceeded},
		{name: "one byte over default limit", outputSize: limit + 1, wantStatus: DelegationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providerCalls := 0
			provider := providerFunc(func(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
				providerCalls++
				events := make(chan llm.Event, 2)
				if providerCalls == 1 {
					events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{
						ID: "large-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"large.txt"}`), Complete: true,
					}}
				} else {
					events <- llm.Event{Kind: llm.TextDelta, Text: "bounded read complete"}
				}
				events <- llm.Event{Kind: llm.StreamEnd}
				close(events)
				errs := make(chan error)
				close(errs)
				return events, errs
			})
			input := ChildRunInput{
				ChildRunID: "output-boundary-child", Work: WorkRef{Kind: WorkSession, SessionID: "output-boundary-session"},
				Task:        DelegationTask{ID: "output-boundary-task", Name: "read file", Instruction: "Read the assigned file."},
				ProjectRoot: "/authorized", Provider: provider, Model: "fixture", Budget: defaults,
				ToolSchemas:     []llm.ToolSchema{{Name: "read_file"}},
				ExecutorFactory: captureChildFactory{exec: productionOutputBoundaryExecutor{outputBytes: tc.outputSize}},
			}
			result := (StreamingChildRunner{}).Run(context.Background(), input)
			if result.Status != tc.wantStatus {
				t.Fatalf("child status=%s error=%q want=%s", result.Status, result.Error, tc.wantStatus)
			}
			if len(result.Summary) > defaults.MaxSummaryBytes {
				t.Fatalf("child summary is %d bytes, exceeds default cap %d", len(result.Summary), defaults.MaxSummaryBytes)
			}
			if tc.outputSize > limit && result.Error != "child tool output limit reached" {
				t.Fatalf("over-limit output error=%q", result.Error)
			}
		})
	}
}
