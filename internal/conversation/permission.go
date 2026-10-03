package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"stable/internal/core"
	"stable/internal/goalrun"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

// AuthorizeOperation is the trusted control-plane entry used by executors.
// Agent payloads must never call it with client-supplied authority.
func (s *Service) AuthorizeOperation(ctx context.Context, authority permission.Authority, operation permission.Operation) (permission.PermissionDecision, error) {
	if s.deps.Store == nil {
		return permission.PermissionDecision{}, errors.New("permission store is unavailable")
	}
	if _, err := sessionlog.SessionPath(s.deps.ProjectRoot, authority.SessionID); err != nil {
		return permission.PermissionDecision{}, err
	}
	scope, err := authority.ScopeDigest()
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	rules, err := s.deps.Store.ListExactRules(ctx, scope)
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	service := permission.PermissionService{NewID: func() string { id, _ := sessionlog.NewID(); return id }}
	if s.deps.PermissionService != nil {
		service = *s.deps.PermissionService
	}
	service.Repository = s.deps.Store
	if service.NewID == nil {
		service.NewID = func() string { id, _ := sessionlog.NewID(); return id }
	}
	service.Policy.Rules = append(rules, service.Policy.Rules...)
	decision, err := service.Authorize(ctx, authority, operation)
	if err == nil && decision.Kind == permission.DecisionAsk {
		s.pushApproval(ctx, decision.ApprovalID, authority.SessionID)
	}
	return decision, err
}

func (s *Service) pendingApprovals(ctx context.Context, sessionID string) ([]permission.ApprovalRequest, error) {
	if s.deps.Store == nil {
		return nil, errors.New("permission store is unavailable")
	}
	if _, err := sessionlog.SessionPath(s.deps.ProjectRoot, sessionID); err != nil {
		return nil, err
	}
	return s.deps.Store.ListPendingApprovals(ctx, sessionID)
}

func (s *Service) resolveApproval(ctx context.Context, msg ClientMsg) (permission.PermissionDecision, error) {
	if s.deps.Store == nil {
		return permission.PermissionDecision{}, errors.New("permission store is unavailable")
	}
	request, err := s.deps.Store.GetApproval(ctx, msg.ApprovalID)
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	if request.SessionID != msg.SessionID || request.Authority.SessionID != msg.SessionID {
		return permission.PermissionDecision{}, errors.New("approval does not belong to this session")
	}
	service := permission.PermissionService{Repository: s.deps.Store}
	if s.deps.PermissionService != nil {
		service = *s.deps.PermissionService
		service.Repository = s.deps.Store
	}
	decision, err := service.ResolveApproval(ctx, request.ID, permission.ApprovalChoice(msg.ApprovalChoice), trustedPrincipal(msg.SessionID), request.Authority, request.Operation)
	if err == nil {
		if request.GoalID != "" {
			s.queuePermissionWake(ctx, request, string(decision.Kind))
		}
		s.mu.Lock()
		delete(s.notifiedApprovals, request.ID)
		prompt := permission.Prompt(request)
		for ch, sub := range s.clients {
			if sub.sessionID == msg.SessionID {
				select {
				case ch <- ServerMsg{Type: "approval_resolved", Approval: &prompt, Decision: &decision}:
				default:
				}
			}
		}
		s.mu.Unlock()
	}
	return decision, err
}

func (s *Service) cancelApproval(ctx context.Context, msg ClientMsg) error {
	if s.deps.Store == nil {
		return errors.New("permission store is unavailable")
	}
	request, err := s.deps.Store.GetApproval(ctx, msg.ApprovalID)
	if err != nil {
		return err
	}
	if request.SessionID != msg.SessionID {
		return errors.New("approval does not belong to this session")
	}
	service := permission.PermissionService{Repository: s.deps.Store}
	if s.deps.PermissionService != nil {
		service = *s.deps.PermissionService
		service.Repository = s.deps.Store
	}
	if err = service.CancelApproval(ctx, msg.ApprovalID, trustedPrincipal(msg.SessionID)); err != nil {
		return err
	}
	if request.GoalID != "" {
		s.queuePermissionWake(ctx, request, "cancelled")
	}
	s.mu.Lock()
	delete(s.notifiedApprovals, request.ID)
	prompt := permission.Prompt(request)
	for ch, sub := range s.clients {
		if sub.sessionID == msg.SessionID {
			select {
			case ch <- ServerMsg{Type: "approval_cancelled", Approval: &prompt}:
			default:
			}
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) queuePermissionWake(ctx context.Context, request permission.ApprovalRequest, result string) {
	if s.deps.Store == nil || request.GoalID == "" {
		return
	}
	if request.Authority.WorkItemID != "" {
		status, reason := "blocked", "approval was denied or cancelled"
		if result == string(permission.DecisionAllow) {
			status, reason = "prepared", "user approval recorded; retrying the same isolated action"
		}
		_ = s.deps.Store.SetActionResult(ctx, request.Authority.WorkItemID, status, "", reason)
	}
	payload, _ := json.Marshal(map[string]string{"approval_id": request.ID, "result": result})
	event, _, err := s.deps.Store.InsertEventIfAbsent(ctx, core.Event{ID: "permission-" + request.ID, GoalID: request.GoalID, Kind: "permission_resolved", Payload: payload})
	if err != nil || s.deps.Temporal == "" {
		return
	}
	if err = goalrun.WakeGoal(ctx, s.deps.Temporal, event.GoalID, event.ID); err == nil {
		_ = s.deps.Store.SetEventStatus(ctx, event.ID, "signaled")
	}
}

func (s *Service) deliverPermissionWakes(ctx context.Context) {
	if s.deps.Store == nil || s.deps.Temporal == "" {
		return
	}
	events, err := s.deps.Store.UnprocessedEvents(ctx)
	if err != nil {
		return
	}
	for _, event := range events {
		if event.Kind != "permission_resolved" {
			continue
		}
		if err = goalrun.WakeGoal(ctx, s.deps.Temporal, event.GoalID, event.ID); err == nil {
			_ = s.deps.Store.SetEventStatus(ctx, event.ID, "signaled")
		}
	}
}

func trustedPrincipal(sessionID string) permission.UserPrincipal {
	return permission.UserPrincipal{SessionID: sessionID, UserID: strconv.Itoa(os.Getuid()), Authenticated: true}
}

func (s *Service) pushApproval(ctx context.Context, id, sessionID string) {
	if id == "" || s.deps.Store == nil {
		return
	}
	request, err := s.deps.Store.GetApproval(ctx, id)
	if err != nil || request.Status != permission.ApprovalPending {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notifiedApprovals == nil {
		s.notifiedApprovals = map[string]bool{}
	}
	if s.notifiedApprovals[request.ID] {
		return
	}
	prompt := permission.Prompt(request)
	for ch, sub := range s.clients {
		if sub.sessionID != sessionID {
			continue
		}
		select {
		case ch <- ServerMsg{Type: "approval_pending", Approval: &prompt}:
			s.notifiedApprovals[request.ID] = true
		default:
		}
	}
}

// CheckApprovalScope is useful to audit the stored authorization before an executor consumes a grant.
func CheckApprovalScope(request permission.ApprovalRequest) error {
	scope, err := request.Authority.ScopeDigest()
	if err != nil {
		return err
	}
	operation, err := request.Operation.Digest()
	if err != nil {
		return err
	}
	if scope != request.ScopeDigest || operation != request.OperationDigest {
		return fmt.Errorf("approval %s scope or operation changed", request.ID)
	}
	return nil
}
