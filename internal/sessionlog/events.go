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
	Ephemeral bool      `json:"ephemeral,omitempty"`
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
	EventPlanMode       = "plan_mode"
	EventPlanApproval   = "plan_approval"
	EventTodo           = "todo_update"
	EventSkillInventory = "skill_inventory"
	EventSkillDelta     = "skill_delta"
	EventSkillInvoked   = "skill_invoked"
	EventHookFired      = "hook_fired"
	EventHookReload     = "hook_reload"
	EventMCPReload      = "mcp_reload"
	EventMCPServer      = "mcp_server"
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

// Plan mode values recorded by plan_mode events.
const (
	PlanModePlan    = "plan"
	PlanModeDefault = "default"
)

// Reasons a session switches between plan and default mode.
const (
	PlanModeReasonUserToggle    = "user_toggle"
	PlanModeReasonPlanApproved  = "plan_approved"
	PlanModeReasonPlanCancelled = "plan_cancelled"
)

// PlanMode records one plan-mode transition. The mode itself is session
// runtime state; the event only keeps the transcript auditable.
type PlanMode struct {
	Mode   string    `json:"mode"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Plan approval request statuses. A request is submitted once and then
// resolved exactly once with a terminal status.
const (
	PlanApprovalSubmitted      = "submitted"
	PlanApprovalApprovedAuto   = "approved_auto"
	PlanApprovalApprovedManual = "approved_manual"
	PlanApprovalFeedback       = "feedback"
	PlanApprovalCancelled      = "cancelled"
)

// PlanApprovalRecord is one lifecycle step of a plan approval request.
type PlanApprovalRecord struct {
	RequestID  string    `json:"request_id"`
	RunID      string    `json:"run_id"`
	PlanPath   string    `json:"plan_path"`
	Status     string    `json:"status"`
	Feedback   string    `json:"feedback,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// MaxTodoTasks bounds the task count of one todo_update snapshot.
const MaxTodoTasks = 100

// TaskSnapshot mirrors one task of the session todo list. It is declared
// here instead of importing internal/todo so sessionlog keeps zero
// business dependencies; the conversation layer converts between them.
type TaskSnapshot struct {
	ID          string            `json:"id"`
	Subject     string            `json:"subject"`
	Description string            `json:"description"`
	ActiveForm  string            `json:"active_form"`
	Status      string            `json:"status"`
	Owner       string            `json:"owner,omitempty"`
	Blocks      []string          `json:"blocks,omitempty"`
	BlockedBy   []string          `json:"blocked_by,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// TodoUpdate is a full task-list snapshot written on every todo change.
type TodoUpdate struct {
	Revision int            `json:"revision"`
	Tasks    []TaskSnapshot `json:"tasks"`
}

// MaxSkillListEntries bounds the entry count of one skill_inventory or
// skill_delta event.
const MaxSkillListEntries = 200

// Entries via which a skill can be activated.
const (
	SkillEntrySlash = "slash"
	SkillEntryTool  = "tool"
)

// SkillInfo is the lightweight skill metadata recorded in skill events.
// Skill bodies never enter the log; they travel through the message or
// tool-result events that carry the activation.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	WhenToUse   string `json:"when_to_use"`
	Source      string `json:"source"`
}

// SkillInventory is the skill catalog snapshot written once per session.
type SkillInventory struct {
	Skills []SkillInfo `json:"skills"`
}

// SkillDelta lists skills newly added to the catalog after the inventory.
type SkillDelta struct {
	Added []SkillInfo `json:"added"`
}

// SkillInvoked records one skill activation for the audit trail. Entry is
// SkillEntrySlash or SkillEntryTool.
type SkillInvoked struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Entry  string `json:"entry"`
	Args   string `json:"args,omitempty"`
}

const MaxHookOutput = 8 * 1024

// HookFired records one hook execution that passed its condition.
type HookFired struct {
	HookID   string `json:"hook_id"`
	Event    string `json:"event"`
	Action   string `json:"action"`
	Source   string `json:"source,omitempty"`
	Success  bool   `json:"success"`
	Rejected bool   `json:"rejected,omitempty"`
	Output   string `json:"output,omitempty"`
	RunID    string `json:"run_id,omitempty"`
}

// HookReload records a hooks configuration reload.
type HookReload struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

// MCPReload records an MCP configuration reload. Trigger is "auto" or
// "manual". Rejections lists the server names the reload refused to apply.
type MCPReload struct {
	Before     int      `json:"before"`
	After      int      `json:"after"`
	Rejections []string `json:"rejections,omitempty"`
	Trigger    string   `json:"trigger"`
}

// MaxMCPOutput bounds the size of the free-form text one MCP event may
// carry, mirroring MaxHookOutput. Writers truncate to this limit before
// appending.
const MaxMCPOutput = 8 * 1024

// MCPServer records one MCP server state transition. State is one of
// "connected", "disconnected", or "reload-failed". Error carries the
// failure diagnostic for reload-failed, truncated to MaxMCPOutput by the
// writer.
type MCPServer struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	ToolCount int    `json:"tool_count"`
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
