// Package memory implements bounded instruction discovery and persistent
// user/project memory for Stable runs.
package memory

import (
	"context"
	"time"

	"stable/internal/decision"
)

type MemoryScope string

const (
	ScopeUser    MemoryScope = "user"
	ScopeProject MemoryScope = "project"
)

type MemoryType string

const (
	TypeUser      MemoryType = "user"
	TypeFeedback  MemoryType = "feedback"
	TypeProject   MemoryType = "project"
	TypeReference MemoryType = "reference"
)

type MemoryAction string

const (
	ActionUpsert MemoryAction = "upsert"
	ActionDelete MemoryAction = "delete"
)

func ScopeForType(kind MemoryType) (MemoryScope, bool) {
	switch kind {
	case TypeUser, TypeFeedback:
		return ScopeUser, true
	case TypeProject, TypeReference:
		return ScopeProject, true
	default:
		return "", false
	}
}

type InstructionSource struct {
	Path     string
	Priority int
	Content  string
}

type LoadIssue struct {
	Path   string
	Reason string
}

type MemoryHeader struct {
	Scope       MemoryScope `json:"scope" yaml:"scope"`
	Type        MemoryType  `json:"type" yaml:"type"`
	Filename    string      `json:"filename" yaml:"filename"`
	Name        string      `json:"name" yaml:"name"`
	Description string      `json:"description" yaml:"description"`
	UpdatedAt   time.Time   `json:"updated_at" yaml:"updated_at"`
}

type MemoryEntry struct {
	MemoryHeader
	Body string `json:"body" yaml:"-"`
}

type MemoryRef struct {
	Scope    MemoryScope `json:"scope"`
	Filename string      `json:"filename"`
}

type MemoryChange struct {
	Action      MemoryAction `json:"action"`
	Scope       MemoryScope  `json:"scope"`
	Type        MemoryType   `json:"type"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Body        string       `json:"body"`
}

type RunMemoryContext struct {
	InstructionText  string
	UserIndex        string
	ProjectIndex     string
	Selected         []MemoryEntry
	Issues           []LoadIssue
	UserTruncated    bool
	ProjectTruncated bool
	ExtractedThrough uint64
}

type ConversationText struct {
	Seq  uint64
	Kind string // session_text or goal_reply
	Text string
}

type RunCompletion struct {
	ProjectRoot          string
	SessionID            string
	RunID                string
	WorkKind             string
	Messages             []ConversationText
	ThroughSeq           uint64
	CompletedAt          time.Time
	MainAgentWroteMemory bool
}

type ExtractionInput struct {
	WorkKind string
	Messages []ConversationText
	Existing []MemoryHeader
}

type ConsolidationInput struct {
	ActiveSessions int
	UserEntries    []MemoryEntry
	ProjectEntries []MemoryEntry
}

type WorkerState struct {
	SessionCursors             map[string]uint64 `json:"session_cursors"`
	LastConsolidatedAt         time.Time         `json:"last_consolidated_at,omitempty"`
	SessionsSinceConsolidation map[string]bool   `json:"sessions_since_consolidation,omitempty"`
}

type ChatModel interface {
	GenerateChat(context.Context, []decision.ChatMessage) (string, error)
}

type Selector interface {
	Select(context.Context, string, []MemoryHeader) ([]MemoryRef, error)
}

type Processor interface {
	Extract(context.Context, ExtractionInput) ([]MemoryChange, error)
	Consolidate(context.Context, ConsolidationInput) ([]MemoryChange, error)
}

type Manager interface {
	PrepareRun(ctx context.Context, projectRoot, workDir, query string) (RunMemoryContext, error)
	List(ctx context.Context, projectRoot string) ([]MemoryHeader, error)
	Read(ctx context.Context, projectRoot string, scope MemoryScope, filename string) (MemoryEntry, error)
	Save(ctx context.Context, projectRoot string, change MemoryChange) error
	Delete(ctx context.Context, projectRoot string, scope MemoryScope, filename string) error
	Clear(ctx context.Context, projectRoot string, scope MemoryScope) (int, error)
	CompleteRun(ctx context.Context, completion RunCompletion)
	MaybeConsolidate(ctx context.Context, projectRoot string) error
}
