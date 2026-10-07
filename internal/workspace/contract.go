// Package workspace owns service-managed isolated project data. Public task
// snapshots deliberately contain no physical paths or permission authority.
package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/permission"
)

const ManifestPolicy = "project-v2"

var (
	ErrUnsafePath           = errors.New("unsafe workspace path")
	ErrOwnership            = errors.New("workspace ownership does not match trusted scope")
	ErrQuota                = errors.New("workspace resource quota exceeded")
	ErrSourceChanged        = errors.New("project changed during workspace snapshot")
	ErrUnsupportedSubmodule = errors.New("workspace submodules are not supported")
	ErrQueueFull            = errors.New("workspace materializer queue is full")
	ErrClosed               = errors.New("workspace materializer is closed")
	ErrUnavailable          = errors.New("workspace safety capability is unavailable")
)

type State string

const (
	StateCreating    State = "creating"
	StateReady       State = "ready"
	StateWriting     State = "writing"
	StateStopping    State = "stopping"
	StateKept        State = "kept"
	StateExporting   State = "exporting"
	StateExported    State = "exported"
	StateRemoving    State = "removing"
	StateRemoved     State = "removed"
	StateBlocked     State = "blocked"
	StateInterrupted State = "interrupted"
)

func (s State) Valid() bool {
	switch s {
	case StateCreating, StateReady, StateWriting, StateStopping, StateKept, StateExporting, StateExported, StateRemoving, StateRemoved, StateBlocked, StateInterrupted:
		return true
	default:
		return false
	}
}

// Scope must be constructed by an authenticated service from a persisted
// session/work item. It must never be decoded directly from model tool args.
type Scope struct {
	ProjectID    string               `json:"project_id"`
	SessionID    string               `json:"session_id"`
	OriginRunID  string               `json:"origin_run_id,omitempty"`
	OriginTaskID string               `json:"origin_task_id,omitempty"`
	Work         agent.WorkRef        `json:"work"`
	Authority    permission.Authority `json:"-"`
}

func (s Scope) Validate() error {
	if !ValidID(s.ProjectID) || !ValidID(s.SessionID) || s.Work.SessionID != s.SessionID {
		return ErrOwnership
	}
	for _, id := range []string{s.OriginRunID, s.OriginTaskID, s.Work.GoalID, s.Work.WorkItemID} {
		if id != "" && !ValidID(id) {
			return ErrOwnership
		}
	}
	switch s.Work.Kind {
	case agent.WorkSession:
		if s.Work.GoalID != "" || s.Work.WorkItemID != "" {
			return ErrOwnership
		}
	case agent.WorkGoal:
		if s.Work.GoalID == "" {
			return ErrOwnership
		}
	default:
		return ErrOwnership
	}
	return nil
}

func (s Scope) ValidateAuthority() error {
	if err := s.Validate(); err != nil {
		return err
	}
	a := s.Authority
	if !ValidID(a.RunID) || a.SessionID != s.SessionID || a.GoalID != s.Work.GoalID || a.WorkItemID != s.Work.WorkItemID {
		return ErrOwnership
	}
	if !filepath.IsAbs(a.AllowedRoot) || !filepath.IsAbs(a.CandidateRoot) || a.FormalRoot != "" && !filepath.IsAbs(a.FormalRoot) {
		return ErrUnsafePath
	}
	return nil
}

func (s Scope) SameOwner(other Scope) bool {
	return s.ProjectID == other.ProjectID && s.SessionID == other.SessionID && s.Work == other.Work
}

type Snapshot struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	SessionID       string   `json:"session_id"`
	State           State    `json:"state"`
	WriterRunID     string   `json:"writer_run_id,omitempty"`
	CandidateID     string   `json:"candidate_id,omitempty"`
	Generation      uint64   `json:"generation"`
	Cursor          uint64   `json:"cursor"`
	BaselineDigest  string   `json:"baseline_digest,omitempty"`
	FormalDigest    string   `json:"formal_digest,omitempty"`
	WorkspaceDigest string   `json:"workspace_digest,omitempty"`
	ChangedFiles    int      `json:"changed_files"`
	ConflictCount   int      `json:"conflict_count"`
	Conflicts       []string `json:"conflicts,omitempty"`
	PreviewID       string   `json:"preview_id,omitempty"`
	ConflictNext    string   `json:"conflict_next,omitempty"`
	ResolutionID    string   `json:"resolution_id,omitempty"`
	ResolvedCount   int      `json:"resolved_count,omitempty"`
	DiscardID       string   `json:"discard_id,omitempty"`
	DiscardDigest   string   `json:"discard_digest,omitempty"`
	DiscardPaths    []string `json:"discard_paths,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// Paths are derived internally from random workspace IDs. They are never
// serialized into a public snapshot or supplied as a protocol argument.
type Paths struct {
	Root, FormalRoot, Baseline, Repository, Checkout, Run, Journal string
}

type WriterLease struct {
	WorkspaceID string               `json:"workspace_id"`
	RunID       string               `json:"run_id"`
	Generation  uint64               `json:"generation"`
	Authority   permission.Authority `json:"-"`
	Paths       Paths                `json:"-"`
	Scope       Scope                `json:"-"`
}

// WriteReservation bounds growth before a file tool mutates a leased checkout
// and reconciles actual allocated bytes after the tool exits.
type WriteReservation interface {
	Commit(context.Context) error
	Release()
}

type WriterAccounting interface {
	ReserveWriterWrite(context.Context, WriterLease, int64) (WriteReservation, error)
}

type Service interface {
	Create(context.Context, Scope, string) (Snapshot, error)
	Get(context.Context, Scope, string) (Snapshot, error)
	List(context.Context, Scope, uint64, int) ([]Snapshot, error)
	Enter(context.Context, Scope, string) (Snapshot, error)
	Exit(context.Context, Scope) (Snapshot, error)
	Keep(context.Context, Scope, string) (Snapshot, error)
	AcquireWriter(context.Context, Scope, string, string) (WriterLease, error)
	StopWriter(context.Context, Scope, string) (Snapshot, error)
	Preview(context.Context, Scope, string) (Snapshot, error)
	Export(context.Context, Scope, string) (Snapshot, error)
	RemoveClean(context.Context, Scope, string) (Snapshot, error)
}

type Limits struct {
	MaxFiles          int
	MaxEntries        int
	MaxSnapshotBytes  int64
	MaxFileBytes      int64
	MaxWorkspaceBytes int64
	MaxProjectBytes   int64
	MaxWorkspaces     int
	QueueCapacity     int
	MaxDuration       time.Duration
}

func DefaultLimits() Limits {
	return Limits{MaxFiles: 20_000, MaxEntries: 80_000, MaxSnapshotBytes: 128 << 20, MaxFileBytes: 16 << 20,
		MaxWorkspaceBytes: 512 << 20, MaxProjectBytes: 2 << 30, MaxWorkspaces: 16, QueueCapacity: 8, MaxDuration: 3 * time.Minute}
}

// Normalized only permits smaller limits than the approved defaults.
func (l Limits) Normalized() Limits {
	d := DefaultLimits()
	if l.MaxFiles > 0 {
		d.MaxFiles = min(d.MaxFiles, l.MaxFiles)
	}
	if l.MaxEntries > 0 {
		d.MaxEntries = min(d.MaxEntries, l.MaxEntries)
	}
	if l.MaxSnapshotBytes > 0 {
		d.MaxSnapshotBytes = min(d.MaxSnapshotBytes, l.MaxSnapshotBytes)
	}
	if l.MaxFileBytes > 0 {
		d.MaxFileBytes = min(d.MaxFileBytes, l.MaxFileBytes)
	}
	if l.MaxWorkspaceBytes > 0 {
		d.MaxWorkspaceBytes = min(d.MaxWorkspaceBytes, l.MaxWorkspaceBytes)
	}
	if l.MaxProjectBytes > 0 {
		d.MaxProjectBytes = min(d.MaxProjectBytes, l.MaxProjectBytes)
	}
	if l.MaxWorkspaces > 0 {
		d.MaxWorkspaces = min(d.MaxWorkspaces, l.MaxWorkspaces)
	}
	if l.QueueCapacity > 0 {
		d.QueueCapacity = min(d.QueueCapacity, l.QueueCapacity)
	}
	if l.MaxDuration > 0 {
		d.MaxDuration = min(d.MaxDuration, l.MaxDuration)
	}
	return d
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

func ValidateLabel(label string) error {
	if strings.TrimSpace(label) == "" || len(label) > 64 || !utf8.ValidString(label) {
		return errors.New("workspace label must contain 1–64 UTF-8 bytes")
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return errors.New("workspace label contains a control character")
		}
	}
	return nil
}
