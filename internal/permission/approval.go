package permission

import (
	"context"
	"errors"
	"time"
)

type ApprovalStatus string

const (
	ApprovalPending     ApprovalStatus = "pending"
	ApprovalAllowedOnce ApprovalStatus = "allowed_once"
	ApprovalSaved       ApprovalStatus = "saved"
	ApprovalDenied      ApprovalStatus = "denied"
	ApprovalCancelled   ApprovalStatus = "cancelled"
	ApprovalExpired     ApprovalStatus = "expired"
)

type ApprovalChoice string

const (
	ChoiceAllowOnce ApprovalChoice = "allow_once"
	ChoiceSaveRule  ApprovalChoice = "save_rule"
	ChoiceDeny      ApprovalChoice = "deny"
)

type ApprovalRequest struct {
	ID              string         `json:"id"`
	RunID           string         `json:"run_id"`
	SessionID       string         `json:"session_id"`
	GoalID          string         `json:"goal_id,omitempty"`
	WorkItemID      string         `json:"work_item_id,omitempty"`
	Operation       Operation      `json:"operation"`
	Authority       Authority      `json:"authority"`
	Reason          string         `json:"reason"`
	OperationDigest string         `json:"operation_digest"`
	ScopeDigest     string         `json:"scope_digest"`
	Status          ApprovalStatus `json:"status"`
	CreatedAt       time.Time      `json:"created_at"`
	ExpiresAt       time.Time      `json:"expires_at"`
}

// ApprovalPrompt exposes only fields needed for a user decision. Raw operation
// parameters remain in the trusted store and are never sent to the TUI.
type ApprovalPrompt struct {
	ID          string         `json:"id"`
	RunID       string         `json:"run_id"`
	SessionID   string         `json:"session_id"`
	GoalID      string         `json:"goal_id,omitempty"`
	WorkItemID  string         `json:"work_item_id,omitempty"`
	Kind        OperationKind  `json:"kind"`
	Name        string         `json:"name"`
	Target      string         `json:"target,omitempty"`
	ScopeDigest string         `json:"scope_digest"`
	AllowedRoot string         `json:"allowed_root"`
	Reason      string         `json:"reason"`
	Status      ApprovalStatus `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

func Prompt(r ApprovalRequest) ApprovalPrompt {
	return ApprovalPrompt{ID: r.ID, RunID: r.RunID, SessionID: r.SessionID, GoalID: r.GoalID, WorkItemID: r.WorkItemID, Kind: r.Operation.Kind, Name: r.Operation.Name, Target: r.Operation.Target, ScopeDigest: r.ScopeDigest, AllowedRoot: r.Authority.AllowedRoot, Reason: r.Reason, Status: r.Status, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}
}

type UserPrincipal struct {
	SessionID     string
	UserID        string
	Authenticated bool
}

type ApprovalRepository interface {
	CreateApproval(context.Context, ApprovalRequest) error
	GetApproval(context.Context, string) (ApprovalRequest, error)
	GetApprovalForOperation(context.Context, string, string, string) (ApprovalRequest, bool, error)
	ResolveApproval(context.Context, string, ApprovalStatus, string, string, *ExactRule) error
	CancelApproval(context.Context, string, string) error
	ConsumeApproval(context.Context, string, string, string) error
	RecordPermissionDecision(context.Context, PermissionDecision, Authority, Operation) error
}

type PermissionService struct {
	Policy      Policy
	Repository  ApprovalRepository
	NewID       func() string
	Now         func() time.Time
	ApprovalTTL time.Duration
}

func (s PermissionService) Authorize(ctx context.Context, authority Authority, operation Operation) (PermissionDecision, error) {
	decision := s.Policy.Decide(authority, operation)
	if decision.Kind == DecisionAsk {
		if s.Repository == nil || s.NewID == nil {
			return PermissionDecision{}, errors.New("permission approval storage is unavailable")
		}
		now := time.Now()
		if s.Now != nil {
			now = s.Now()
		}
		ttl := s.ApprovalTTL
		if ttl <= 0 {
			ttl = 15 * time.Minute
		}
		if existing, ok, err := s.Repository.GetApprovalForOperation(ctx, authority.RunID, decision.OperationDigest, decision.ScopeDigest); err != nil {
			return PermissionDecision{}, err
		} else if ok {
			switch existing.Status {
			case ApprovalPending:
				if !existing.ExpiresAt.After(now) {
					if err = s.Repository.ResolveApproval(ctx, existing.ID, ApprovalExpired, decision.ScopeDigest, decision.OperationDigest, nil); err != nil {
						return PermissionDecision{}, err
					}
					decision.Kind, decision.Reason, decision.ApprovalID = DecisionDeny, "approval expired", existing.ID
				} else {
					decision.ApprovalID = existing.ID
				}
			case ApprovalAllowedOnce:
				if err = s.Repository.ConsumeApproval(ctx, existing.ID, decision.ScopeDigest, decision.OperationDigest); err != nil {
					return PermissionDecision{}, err
				}
				decision.Kind, decision.Reason, decision.ApprovalID = DecisionAllow, "approved once by user", existing.ID
			case ApprovalDenied, ApprovalCancelled, ApprovalExpired:
				decision.Kind, decision.Reason, decision.ApprovalID = DecisionDeny, "operation was previously denied or cancelled", existing.ID
			case ApprovalSaved:
				decision.Kind, decision.Reason, decision.ApprovalID = DecisionAllow, "exact operation rule saved", existing.ID
			default:
				return PermissionDecision{}, errors.New("approval has an invalid stored state")
			}
		} else {
			request := ApprovalRequest{ID: s.NewID(), RunID: authority.RunID, SessionID: authority.SessionID, GoalID: authority.GoalID, WorkItemID: authority.WorkItemID, Operation: operation, Authority: authority, Reason: decision.Reason, OperationDigest: decision.OperationDigest, ScopeDigest: decision.ScopeDigest, Status: ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(ttl)}
			if request.ID == "" {
				return PermissionDecision{}, errors.New("could not create approval ID")
			}
			if err := s.Repository.CreateApproval(ctx, request); err != nil {
				return PermissionDecision{}, err
			}
			decision.ApprovalID = request.ID
		}
	}
	if s.Repository != nil {
		if err := s.Repository.RecordPermissionDecision(ctx, decision, authority, operation); err != nil {
			return PermissionDecision{}, err
		}
	}
	return decision, nil
}

func (s PermissionService) ResolveApproval(ctx context.Context, id string, choice ApprovalChoice, principal UserPrincipal, authority Authority, operation Operation) (PermissionDecision, error) {
	if !principal.Authenticated || principal.UserID == "" || principal.SessionID != authority.SessionID {
		return PermissionDecision{}, errors.New("approval requires the authenticated user from the owning session")
	}
	if s.Repository == nil {
		return PermissionDecision{}, errors.New("permission approval storage is unavailable")
	}
	request, err := s.Repository.GetApproval(ctx, id)
	if err != nil {
		return PermissionDecision{}, err
	}
	scope, err := authority.ScopeDigest()
	if err != nil {
		return PermissionDecision{}, err
	}
	opDigest, err := operation.Digest()
	if err != nil {
		return PermissionDecision{}, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if request.Status != ApprovalPending || request.SessionID != principal.SessionID || request.ScopeDigest != scope || request.OperationDigest != opDigest || request.RunID != authority.RunID {
		return PermissionDecision{}, errors.New("approval is expired or no longer matches the operation scope")
	}
	if !request.ExpiresAt.After(now) {
		if err = s.Repository.ResolveApproval(ctx, id, ApprovalExpired, scope, opDigest, nil); err != nil {
			return PermissionDecision{}, err
		}
		expired := PermissionDecision{Kind: DecisionDeny, Reason: "approval expired", UserID: principal.UserID, ScopeDigest: scope, OperationDigest: opDigest, ApprovalID: id}
		if err = s.Repository.RecordPermissionDecision(ctx, expired, authority, operation); err != nil {
			return PermissionDecision{}, err
		}
		return expired, errors.New("approval has expired")
	}
	status := ApprovalDenied
	var rule *ExactRule
	decision := PermissionDecision{Kind: DecisionDeny, Reason: "user denied operation", ScopeDigest: scope, OperationDigest: opDigest, ApprovalID: id}
	decision.UserID = principal.UserID
	switch choice {
	case ChoiceAllowOnce:
		status = ApprovalAllowedOnce
		decision.Kind = DecisionAllow
		decision.Reason = "approved once by user"
	case ChoiceSaveRule:
		status = ApprovalSaved
		decision.Kind = DecisionAllow
		decision.Reason = "exact operation rule saved"
		parametersDigest, err := digest(operation.Parameters)
		if err != nil {
			return PermissionDecision{}, err
		}
		target, err := ruleTarget(authority, operation)
		if err != nil {
			return PermissionDecision{}, err
		}
		rule = &ExactRule{Effect: EffectAllow, Kind: operation.Kind, Name: operation.Name, Target: target, ParametersDigest: parametersDigest, ScopeDigest: scope, Protocol: operation.Protocol, Host: operation.Host, Port: operation.Port}
	case ChoiceDeny:
		parametersDigest, err := digest(operation.Parameters)
		if err != nil {
			return PermissionDecision{}, err
		}
		target, err := ruleTarget(authority, operation)
		if err != nil {
			return PermissionDecision{}, err
		}
		rule = &ExactRule{Effect: EffectDeny, Kind: operation.Kind, Name: operation.Name, Target: target, ParametersDigest: parametersDigest, ScopeDigest: scope, Protocol: operation.Protocol, Host: operation.Host, Port: operation.Port}
	default:
		return PermissionDecision{}, errors.New("unknown approval choice")
	}
	if err = s.Repository.ResolveApproval(ctx, id, status, scope, opDigest, rule); err != nil {
		return PermissionDecision{}, err
	}
	if err = s.Repository.RecordPermissionDecision(ctx, decision, authority, operation); err != nil {
		return PermissionDecision{}, err
	}
	return decision, nil
}

func (s PermissionService) ConsumeApproval(ctx context.Context, id string, authority Authority, operation Operation) error {
	if s.Repository == nil {
		return errors.New("permission approval storage is unavailable")
	}
	scope, err := authority.ScopeDigest()
	if err != nil {
		return err
	}
	opDigest, err := operation.Digest()
	if err != nil {
		return err
	}
	return s.Repository.ConsumeApproval(ctx, id, scope, opDigest)
}

func (s PermissionService) CancelApproval(ctx context.Context, id string, principal UserPrincipal) error {
	if !principal.Authenticated || principal.UserID == "" {
		return errors.New("approval cancellation requires an authenticated user")
	}
	if s.Repository == nil {
		return errors.New("permission approval storage is unavailable")
	}
	request, err := s.Repository.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	if request.SessionID != principal.SessionID {
		return errors.New("approval does not belong to this session")
	}
	if err = s.Repository.CancelApproval(ctx, id, principal.SessionID); err != nil {
		return err
	}
	decision := PermissionDecision{Kind: DecisionDeny, Reason: "approval cancelled by user", UserID: principal.UserID, ScopeDigest: request.ScopeDigest, OperationDigest: request.OperationDigest, ApprovalID: id}
	return s.Repository.RecordPermissionDecision(ctx, decision, request.Authority, request.Operation)
}
