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

type Boundary struct {
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	Summary string `json:"summary"`
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
)

type Transcript struct {
	Session SessionInfo `json:"session"`
	Events  []Event     `json:"events"`
}
