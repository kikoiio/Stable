package sessionlog

import "time"

const SchemaVersion = 1

type Event struct {
	SchemaVersion int       `json:"schema_version"`
	SessionID     string    `json:"session_id"`
	Seq           uint64    `json:"seq"`
	At            time.Time `json:"at"`
	Type          string    `json:"type"`
	Data          any       `json:"data,omitempty"`
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	Role       string `json:"role"`
	Text       string `json:"text"`
	Kind       string `json:"kind,omitempty"`
	Proposal   string `json:"proposal_id,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolInput  any    `json:"tool_input,omitempty"`
	ToolResult any    `json:"tool_result,omitempty"`
}

const (
	BoundaryScopeSession = "session"
	BoundaryScopeRun     = "run"
)

type Boundary struct {
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	Summary string `json:"summary"`
	// Scope selects whether FromSeq/ToSeq are session or run sequence
	// numbers. Empty means session scope for backward compatibility with
	// boundaries written before the scope field existed.
	Scope string `json:"scope,omitempty"`
	// RunID is required when Scope is run and must be empty otherwise.
	RunID string `json:"run_id,omitempty"`
}

// EffectiveScope normalizes a missing scope to session scope.
func (b Boundary) EffectiveScope() string {
	if b.Scope == "" {
		return BoundaryScopeSession
	}
	return b.Scope
}

type ToolCall struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  any    `json:"input,omitempty"`
}
type ToolResult struct {
	CallID string `json:"call_id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

const (
	EventSessionCreated = "session_created"
	EventActivity       = "activity"
	EventMessage        = "message"
	EventProposal       = "proposal_reference"
	EventToolCall       = "tool_call"
	EventToolResult     = "tool_result"
	EventBoundary       = "compaction_boundary"
	EventRunStarted     = "run_started"
	EventRunEvent       = "run_event"
	EventSnapshot       = "candidate_snapshot"
	EventRewind         = "candidate_rewind"
	EventQuestion       = "pending_question"
	EventReply          = "question_reply"
)

// SnapshotRef records a candidate file snapshot owned by this session.
type SnapshotRef struct {
	SnapshotID  string    `json:"snapshot_id"`
	SessionID   string    `json:"session_id"`
	CandidateID string    `json:"candidate_id"`
	RunID       string    `json:"run_id,omitempty"`
	Label       string    `json:"label,omitempty"`
	Digest      string    `json:"digest"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	RewindPending   = "pending"
	RewindCompleted = "completed"
	RewindFailed    = "failed"
)

// RewindRecord reports the state of one candidate rewind attempt.
type RewindRecord struct {
	SnapshotID  string    `json:"snapshot_id"`
	CandidateID string    `json:"candidate_id"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	QuestionPending = "pending"
	QuestionReplied = "replied"
)

// PendingQuestion is a question an agent asked and still expects the user
// to answer. The field set is stable so the M06 question UI can reuse it.
type PendingQuestion struct {
	QuestionID string    `json:"question_id"`
	WorkRef    string    `json:"work_ref"`
	SessionID  string    `json:"session_id"`
	RunID      string    `json:"run_id"`
	Prompt     string    `json:"prompt"`
	CreatedAt  time.Time `json:"created_at"`
	Status     string    `json:"status"`
}

// QuestionReply answers one pending question exactly once.
type QuestionReply struct {
	QuestionID string    `json:"question_id"`
	ReplyText  string    `json:"reply_text"`
	RepliedAt  time.Time `json:"replied_at"`
}

type RunStarted struct {
	RunID      string `json:"run_id"`
	WorkKind   string `json:"work_kind"`
	GoalID     string `json:"goal_id,omitempty"`
	WorkItemID string `json:"work_item_id,omitempty"`
	Intent     string `json:"intent"`
}

type RunEvent struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	RunSeq    uint64    `json:"run_seq"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Payload   any       `json:"payload,omitempty"`
}

type Transcript struct {
	Session SessionInfo `json:"session"`
	Events  []Event     `json:"events"`
}
