package agent

import (
	"context"
	"encoding/json"
	"time"

	"stable/internal/llm"
)

type WorkKind string

const (
	WorkSession WorkKind = "session"
	WorkGoal    WorkKind = "goal"
)

type WorkRef struct {
	Kind       WorkKind `json:"kind"`
	SessionID  string   `json:"session_id"`
	GoalID     string   `json:"goal_id,omitempty"`
	WorkItemID string   `json:"work_item_id,omitempty"`
}

type ExecutionRequest struct {
	RunID            string          `json:"run_id"`
	Work             WorkRef         `json:"work"`
	Intent           string          `json:"intent"`
	Messages         []llm.Message   `json:"messages"`
	ProviderName     string          `json:"provider_name"`
	Model            string          `json:"model"`
	BaselineVersion  string          `json:"baseline_version,omitempty"`
	AllowedScope     []string        `json:"allowed_scope,omitempty"`
	ResourceBounds   json.RawMessage `json:"resource_bounds,omitempty"`
	PermissionBounds json.RawMessage `json:"permission_bounds,omitempty"`
}

type EventKind string

const (
	EventTextDelta        EventKind = "text_delta"
	EventThinkingDelta    EventKind = "thinking_delta"
	EventThinkingComplete EventKind = "thinking_complete"
	EventToolCallStart    EventKind = "tool_call_start"
	EventToolCallDelta    EventKind = "tool_call_delta"
	EventToolCallComplete EventKind = "tool_call_complete"
	EventToolExecStart    EventKind = "tool_exec_start"
	EventToolExecResult   EventKind = "tool_exec_result"
	EventAwaitingApproval EventKind = "awaiting_approval"
	EventBudgetExhausted  EventKind = "budget_exhausted"
	EventUsage            EventKind = "usage"
	EventRetry            EventKind = "retry"
	EventError            EventKind = "error"
	EventTerminal         EventKind = "terminal"
)

type ExecutionEvent struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	SessionID string          `json:"session_id"`
	RunSeq    uint64          `json:"run_seq"`
	At        time.Time       `json:"at"`
	Kind      EventKind       `json:"kind"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type RunStatus string

const (
	RunCompleted       RunStatus = "completed"
	RunCancelled       RunStatus = "cancelled"
	RunFailed          RunStatus = "failed"
	RunAwaitingTools   RunStatus = "awaiting_tools"
	RunBudgetExhausted RunStatus = "budget_exhausted"
)

type RunOutcome struct {
	RunID  string             `json:"run_id"`
	Status RunStatus          `json:"status"`
	Error  *llm.ProviderError `json:"error,omitempty"`
}

type RunHandle struct {
	Events <-chan ExecutionEvent
	Done   <-chan RunOutcome
}

type Publisher interface {
	Publish(ExecutionEvent) error
}

type Runner interface {
	Start(context.Context, ExecutionRequest) (*RunHandle, error)
	Cancel(runID string) error
}
