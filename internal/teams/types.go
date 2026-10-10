// Package teams contains bounded domain rules for read-only team collaboration.
// It has no provider, persistence or external terminal backend dependencies.
package teams

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Lead = "lead"
const BackendInProcess = "in-process"

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func NormalizeName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !namePattern.MatchString(name) {
		return "", errors.New("team names must be 1-64 ASCII letters, digits, underscores or hyphens")
	}
	return name, nil
}

func NormalizeMemberName(name string) (string, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return "", err
	}
	if name == Lead {
		return "", errors.New("lead is reserved for the trusted parent")
	}
	return name, nil
}

func ValidateID(id string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) {
		return errors.New("team ID is missing or exceeds its limit")
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return errors.New("team ID contains unsupported characters")
	}
	return nil
}

func ValidateText(text string, max int, required bool) error {
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 || len(text) > max || (required && strings.TrimSpace(text) == "") {
		return errors.New("team text is invalid or exceeds its byte limit")
	}
	return nil
}

// Scope is immutable ownership metadata. Team IDs never grant access without
// the same session/work binding. Roots are supplied by the trusted host.
type Scope struct {
	SessionID    string `json:"session_id"`
	WorkKind     string `json:"work_kind"`
	GoalID       string `json:"goal_id,omitempty"`
	WorkItemID   string `json:"work_item_id,omitempty"`
	ProjectRoot  string `json:"project_root"`
	ProviderName string `json:"provider_name,omitempty"`
}

func (s Scope) Validate() error {
	if ValidateID(s.SessionID) != nil || !filepath.IsAbs(s.ProjectRoot) || strings.IndexByte(s.ProjectRoot, 0) >= 0 {
		return errors.New("team scope requires a session and absolute authorized root")
	}
	switch s.WorkKind {
	case "session":
		if s.GoalID != "" || s.WorkItemID != "" {
			return errors.New("session team scope cannot contain goal work IDs")
		}
	case "goal":
		if ValidateID(s.GoalID) != nil || ValidateID(s.WorkItemID) != nil {
			return errors.New("goal team scope requires goal and work item IDs")
		}
	default:
		return errors.New("invalid team work kind")
	}
	if len(s.ProviderName) > 256 || strings.IndexFunc(s.ProviderName, unicode.IsControl) >= 0 {
		return errors.New("invalid team provider name")
	}
	return nil
}

func (s Scope) SameWork(other Scope) bool {
	return s.SessionID == other.SessionID && s.WorkKind == other.WorkKind && s.GoalID == other.GoalID && s.WorkItemID == other.WorkItemID
}

func (s Scope) Matches(other Scope) bool {
	return s.Validate() == nil && other.Validate() == nil && s.SameWork(other) && filepath.Clean(s.ProjectRoot) == filepath.Clean(other.ProjectRoot) && s.ProviderName == other.ProviderName
}

type TeamStatus string

const (
	TeamOpen    TeamStatus = "open"
	TeamClosing TeamStatus = "closing"
	TeamClosed  TeamStatus = "closed"
)

func ValidateTeamTransition(from, to TeamStatus) error {
	if from == to && (to == TeamOpen || to == TeamClosing || to == TeamClosed) {
		return nil
	}
	if (from == "" && to == TeamOpen) || (from == TeamOpen && to == TeamClosing) || (from == TeamClosing && to == TeamClosed) {
		return nil
	}
	return errors.New("invalid team lifecycle transition")
}

type MemberStatus string

const (
	MemberCreated         MemberStatus = "created"
	MemberWaitingCapacity MemberStatus = "waiting_capacity"
	MemberQueued          MemberStatus = "queued"
	MemberRunning         MemberStatus = "running"
	MemberIdle            MemberStatus = "idle"
	MemberAwaitingPlan    MemberStatus = "awaiting_plan"
	MemberStopping        MemberStatus = "stopping"
	MemberInterrupted     MemberStatus = "interrupted"
	MemberBudgetExhausted MemberStatus = "budget_exhausted"
	MemberStopped         MemberStatus = "stopped"
)

func (s MemberStatus) IsTerminal() bool { return s == MemberStopped || s == MemberBudgetExhausted }
func (s MemberStatus) HasTurn() bool {
	return s == MemberQueued || s == MemberRunning || s == MemberStopping
}

func ValidateMemberTransition(from, to MemberStatus) error {
	allowed := map[MemberStatus][]MemberStatus{
		"":                    {MemberCreated},
		MemberCreated:         {MemberQueued, MemberWaitingCapacity, MemberInterrupted, MemberStopped, MemberBudgetExhausted},
		MemberWaitingCapacity: {MemberQueued, MemberInterrupted, MemberStopped, MemberBudgetExhausted},
		MemberQueued:          {MemberRunning, MemberStopping, MemberInterrupted, MemberIdle, MemberAwaitingPlan, MemberBudgetExhausted},
		MemberRunning:         {MemberIdle, MemberAwaitingPlan, MemberStopping, MemberInterrupted, MemberBudgetExhausted},
		MemberIdle:            {MemberQueued, MemberWaitingCapacity, MemberAwaitingPlan, MemberInterrupted, MemberStopped, MemberBudgetExhausted},
		MemberAwaitingPlan:    {MemberIdle, MemberQueued, MemberWaitingCapacity, MemberInterrupted, MemberStopped, MemberBudgetExhausted},
		MemberStopping:        {MemberStopped, MemberInterrupted},
		MemberInterrupted:     {MemberIdle, MemberQueued, MemberWaitingCapacity, MemberStopped, MemberBudgetExhausted},
		MemberBudgetExhausted: {MemberStopped},
		MemberStopped:         {},
	}
	if _, ok := allowed[from]; !ok {
		return errors.New("invalid member lifecycle state")
	}
	if from == to && from != "" {
		return nil
	}
	for _, next := range allowed[from] {
		if to == next {
			return nil
		}
	}
	return errors.New("invalid member lifecycle transition")
}

type Team struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Scope        Scope      `json:"scope"`
	CreatorRunID string     `json:"creator_run_id,omitempty"`
	Status       TeamStatus `json:"status"`
	Revision     uint64     `json:"revision"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Member struct {
	ID           string       `json:"id"`
	TeamID       string       `json:"team_id"`
	WorkspaceID  string       `json:"workspace_id,omitempty"`
	Name         string       `json:"name"`
	AgentName    string       `json:"agent_name"`
	RoleHash     string       `json:"role_hash"`
	Model        string       `json:"model"`
	Tools        []string     `json:"tools"`
	Status       MemberStatus `json:"status"`
	PlanRequired bool         `json:"plan_required"`
	PlanApproved bool         `json:"plan_approved"`
	TurnID       string       `json:"turn_id,omitempty"`
	RunID        string       `json:"run_id,omitempty"`
	OriginRunID  string       `json:"origin_run_id,omitempty"`
	Summary      string       `json:"summary,omitempty"`
	Error        string       `json:"error,omitempty"`
	Budget       Budget       `json:"budget"`
	Revision     uint64       `json:"revision"`
}

type Message struct {
	ID         string    `json:"id"`
	TeamID     string    `json:"team_id"`
	SenderID   string    `json:"sender_id"`
	Recipients []string  `json:"recipients"`
	Body       string    `json:"body"`
	Seq        uint64    `json:"seq"`
	CreatedAt  time.Time `json:"created_at"`
}

type RequestType string

const (
	RequestPlan     RequestType = "plan_approval"
	RequestShutdown RequestType = "shutdown"
)

type RequestStatus string

const (
	RequestPending  RequestStatus = "pending"
	RequestDeferred RequestStatus = "deferred"
	RequestApproved RequestStatus = "approved"
	RequestRejected RequestStatus = "rejected"
	RequestExpired  RequestStatus = "expired"
)

type Request struct {
	ID          string        `json:"id"`
	TeamID      string        `json:"team_id"`
	MemberID    string        `json:"member_id"`
	Type        RequestType   `json:"type"`
	Status      RequestStatus `json:"status"`
	RequesterID string        `json:"requester_id"`
	ResponderID string        `json:"responder_id"`
	Body        string        `json:"body"`
	Feedback    string        `json:"feedback,omitempty"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Revision    uint64        `json:"revision"`
}

type Actor struct {
	MemberID string
	Lead     bool
}

func (a Actor) Validate() error {
	if a.Lead {
		if a.MemberID != "" && a.MemberID != Lead {
			return ErrPermission
		}
		return nil
	}
	if a.MemberID == Lead || ValidateID(a.MemberID) != nil {
		return ErrPermission
	}
	return nil
}
