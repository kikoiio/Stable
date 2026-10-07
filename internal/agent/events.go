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
	RunDeadline      time.Time       `json:"-"`
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
	// EventCompactionBoundary records one persistent context compaction: the
	// summary replaces the covered run sequence range. The conversation
	// consumer turns it into a run-scope session log boundary.
	EventCompactionBoundary EventKind = "compaction_boundary"
	EventDelegation         EventKind = "delegation_event"
)

// ContextBoundary is the agent-stream form of a compaction boundary.
type ContextBoundary struct {
	RunID   string `json:"run_id"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	Summary string `json:"summary"`
}

// ContextPreparer compacts run messages before a provider request.
// msgSeqs[i] is the run sequence at which messages[i] was completed (0 for
// messages that predate the run). A nil boundary in the result means no
// compaction was needed. Any error aborts the turn visibly instead of
// sending an unprocessed over-budget request.
type ContextPreparer interface {
	PrepareRun(ctx context.Context, runID string, messages []llm.Message, msgSeqs []uint64) (PreparedRun, error)
}

// PreparedRun carries the messages to send and, after a compaction, the
// boundary to publish first. HeadKept counts leading messages retained
// verbatim; TailStart is the index in the original slice where the retained
// tail begins, so the caller can realign message sequence numbers.
type PreparedRun struct {
	Messages  []llm.Message
	HeadKept  int
	TailStart int
	Boundary  *ContextBoundary
}

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
	RunInterrupted     RunStatus = "interrupted"
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

// DelegationEvent is a user-visible lifecycle update. Summary is intended for
// concise stage text only; model reasoning and raw child transcripts are not
// represented by this type.
type DelegationEvent struct {
	SessionID string           `json:"session_id,omitempty"`
	BatchID   string           `json:"batch_id"`
	TaskID    string           `json:"task_id"`
	TaskName  string           `json:"task_name"`
	Status    DelegationStatus `json:"status"`
	Stage     string           `json:"stage,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	Error     string           `json:"error,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type ProgressReporter interface {
	Publish(parentRunID string, event DelegationEvent) error
}

type DelegationProgress func(stage, summary string)
