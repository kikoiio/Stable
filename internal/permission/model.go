package permission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
)

type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModePlan        Mode = "plan"
	ModeBypass      Mode = "bypassPermissions"
)

type OperationKind string

const (
	OpRead    OperationKind = "read"
	OpWrite   OperationKind = "write"
	OpCommand OperationKind = "command"
	OpNetwork OperationKind = "network"
	OpLegacy  OperationKind = "legacy"
)

type Authority struct {
	RunID         string `json:"run_id"`
	SessionID     string `json:"session_id"`
	GoalID        string `json:"goal_id,omitempty"`
	WorkItemID    string `json:"work_item_id,omitempty"`
	AllowedRoot   string `json:"allowed_root"`
	CandidateRoot string `json:"candidate_root"`
	FormalRoot    string `json:"formal_root,omitempty"`
	Mode          Mode   `json:"mode"`
	// PlanFilePath is the session's plan file inside the formal project
	// (.stable/plans/<sessionID>.md); empty means the session is not in plan
	// mode. Like the other run-scoped identifiers it is excluded from the
	// scope digest and re-checked on every operation.
	PlanFilePath string         `json:"plan_file_path,omitempty"`
	Network      []NetworkGrant `json:"network,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
}

type NetworkGrant struct {
	Protocol    string   `json:"protocol"`
	Host        string   `json:"host"`
	Port        uint16   `json:"port"`
	ResolvedIPs []string `json:"resolved_ips,omitempty"`
}

type Operation struct {
	ID         string          `json:"id"`
	Kind       OperationKind   `json:"kind"`
	Name       string          `json:"name"`
	Target     string          `json:"target,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Protocol   string          `json:"protocol,omitempty"`
	Host       string          `json:"host,omitempty"`
	Port       uint16          `json:"port,omitempty"`
}

type DecisionKind string

const (
	DecisionAllow DecisionKind = "allow"
	DecisionDeny  DecisionKind = "deny"
	DecisionAsk   DecisionKind = "ask"
)

type PermissionDecision struct {
	Kind            DecisionKind `json:"kind"`
	Reason          string       `json:"reason"`
	UserID          string       `json:"user_id,omitempty"`
	ScopeDigest     string       `json:"scope_digest"`
	OperationDigest string       `json:"operation_digest"`
	ApprovalID      string       `json:"approval_id,omitempty"`
}

var ErrInvalidAuthority = errors.New("invalid permission authority")

func (a Authority) ScopeDigest() (string, error) {
	if a.RunID == "" || a.SessionID == "" || a.AllowedRoot == "" || a.CandidateRoot == "" {
		return "", ErrInvalidAuthority
	}
	a.AllowedRoot = filepath.Clean(a.AllowedRoot)
	a.CandidateRoot = filepath.Clean(a.CandidateRoot)
	if a.FormalRoot != "" {
		a.FormalRoot = filepath.Clean(a.FormalRoot)
	}
	sort.Slice(a.Network, func(i, j int) bool {
		x, y := a.Network[i], a.Network[j]
		if x.Protocol != y.Protocol {
			return x.Protocol < y.Protocol
		}
		if x.Host != y.Host {
			return x.Host < y.Host
		}
		return x.Port < y.Port
	})
	for i := range a.Network {
		sort.Strings(a.Network[i].ResolvedIPs)
	}
	sort.Strings(a.Capabilities)
	// A saved rule follows the trusted project scope across candidate/run IDs
	// and plan-mode toggles; those transient paths are still checked on every
	// operation before rules.
	a.RunID = ""
	a.WorkItemID = ""
	a.CandidateRoot = ""
	a.PlanFilePath = ""
	return digest(a)
}

func (o Operation) Digest() (string, error) {
	if o.ID == "" || o.Kind == "" || o.Name == "" {
		return "", errors.New("operation ID, kind and name are required")
	}
	return digest(o)
}

func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}
