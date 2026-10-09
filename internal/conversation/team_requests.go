package conversation

import (
	"context"
	"errors"
	"sort"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func (s *Service) SubmitTeamPlan(ctx context.Context, request agent.ExecutionRequest, teamID, body string) (teams.Request, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil || actor.Lead {
		return teams.Request{}, teams.ErrPermission
	}
	if err := teams.ValidateText(body, teams.MaxPlanBytes, true); err != nil {
		return teams.Request{}, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Request{}, err
	}
	if _, currentScope, currentActor, authErr := s.teamOperationScope(ctx, request); authErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Request{}, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Request{}, err
	}
	member := projection.Members[actor.MemberID]
	if !member.PlanRequired || member.PlanApproved || member.Status != teams.MemberRunning {
		return teams.Request{}, errors.New("member is not awaiting an initial plan submission")
	}
	for _, pending := range projection.Requests {
		if pending.TeamID == teamID && pending.MemberID == actor.MemberID && pending.Type == teams.RequestPlan &&
			(pending.Status == teams.RequestPending || pending.Status == teams.RequestDeferred) {
			return teams.Request{}, teams.ErrCapacity
		}
	}
	return s.createTeamRequest(root, team, request.RunID, actorID(actor), actor.MemberID, teams.RequestPlan, body)
}

func (s *Service) RequestTeamShutdown(ctx context.Context, request agent.ExecutionRequest, teamID, memberID string) (teams.Request, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil || !actor.Lead || teams.ValidateID(memberID) != nil {
		return teams.Request{}, teams.ErrPermission
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Request{}, err
	}
	if _, currentScope, currentActor, authErr := s.teamOperationScope(ctx, request); authErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Request{}, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Request{}, err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != teamID || member.Status.IsTerminal() || member.Status == teams.MemberStopping {
		return teams.Request{}, teams.ErrNotFound
	}
	for _, pending := range projection.Requests {
		if pending.TeamID == teamID && pending.MemberID == memberID && pending.Type == teams.RequestShutdown &&
			(pending.Status == teams.RequestPending || pending.Status == teams.RequestDeferred) {
			return teams.Request{}, teams.ErrCapacity
		}
	}
	requestFact, err := s.createTeamRequest(root, team, request.RunID, teams.Lead, memberID, teams.RequestShutdown, "")
	if err != nil {
		return teams.Request{}, err
	}
	team.Revision++
	if !member.Status.HasTurn() {
		requestFact.Status = teams.RequestApproved
		requestFact.Revision++
		if err := s.appendTeamRequest(root, team, request.RunID, "service", sessionlog.TeamRequestResponded, requestFact); err != nil {
			return requestFact, err
		}
		team.Revision++
		member.Status = teams.MemberStopped
		member.Revision++
		if err := s.appendTeamMemberState(root, team, request.RunID, "service", member); err != nil {
			return requestFact, err
		}
	}
	return requestFact, nil
}

// StopTeamMember is the explicit local force-stop path. It records an approved
// typed shutdown request and stopping state before canceling the actual pool
// handle; terminal member state is written only after the child exits.
func (s *Service) StopTeamMember(ctx context.Context, sessionID, teamID, memberID string) (teams.Member, error) {
	if s == nil || s.teamScheduler == nil {
		return teams.Member{}, errors.New("team member scheduler is unavailable")
	}
	request, err := s.teamUserRequest(ctx, sessionID, teamID)
	if err != nil {
		return teams.Member{}, err
	}
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil || !actor.Lead {
		return teams.Member{}, teams.ErrPermission
	}
	s.eventMu.Lock()
	if err := ctx.Err(); err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != teamID {
		s.eventMu.Unlock()
		return teams.Member{}, teams.ErrNotFound
	}
	if member.Status == teams.MemberStopping {
		s.eventMu.Unlock()
		return member, nil
	}
	if !member.Status.HasTurn() {
		s.eventMu.Unlock()
		return teams.Member{}, teams.ErrNotFound
	}
	s.teamScheduler.mu.Lock()
	cancel := s.teamScheduler.active[member.TurnID]
	s.teamScheduler.mu.Unlock()
	if cancel == nil {
		s.eventMu.Unlock()
		return teams.Member{}, errors.New("team child stop handle is unavailable")
	}
	shutdown, err := s.createTeamRequest(root, team, "", teams.Lead, member.ID, teams.RequestShutdown, "")
	if err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	team.Revision++
	shutdown.Status = teams.RequestApproved
	shutdown.Revision++
	if err := s.appendTeamRequest(root, team, "", "service", sessionlog.TeamRequestResponded, shutdown); err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	team.Revision++
	member.Status = teams.MemberStopping
	member.Revision++
	if err := s.appendTeamMemberState(root, team, "", "service", member); err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	s.eventMu.Unlock()
	if invalidated := s.teamScheduler.invalidateMember(memberID); invalidated != nil {
		cancel = invalidated
	}
	cancel()
	return member, nil
}

func (s *Service) ListTeamRequests(ctx context.Context, request agent.ExecutionRequest, teamID string, requestedLimit ...int) ([]teams.Request, error) {
	if len(requestedLimit) > 1 {
		return nil, errors.New("team request query accepts at most one limit")
	}
	limit := 0
	if len(requestedLimit) == 1 {
		limit = requestedLimit[0]
	}
	return s.ListTeamRequestsPage(ctx, request, teamID, "", limit)
}

// ListTeamRequestsPage returns a bounded expiry/ID-ordered page after a visible request ID.
func (s *Service) ListTeamRequestsPage(ctx context.Context, request agent.ExecutionRequest, teamID, afterRequestID string, limit int) ([]teams.Request, error) {
	if afterRequestID != "" && teams.ValidateID(afterRequestID) != nil {
		return nil, errors.New("team request cursor is invalid")
	}
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return nil, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, currentScope, currentActor, authErr := s.teamOperationScope(ctx, request); authErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return nil, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return nil, err
	}
	due := make([]teams.Request, 0)
	now := time.Now().UTC()
	for _, item := range projection.Requests {
		if (item.Status == teams.RequestPending || item.Status == teams.RequestDeferred) && !now.Before(item.ExpiresAt) {
			due = append(due, item)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ExpiresAt.Equal(due[j].ExpiresAt) {
			return due[i].ID < due[j].ID
		}
		return due[i].ExpiresAt.Before(due[j].ExpiresAt)
	})
	for _, item := range due {
		team, item, err = s.expireTeamRequest(root, team, item)
		if err != nil {
			return nil, err
		}
		projection.Requests[item.ID] = item
	}
	if err := s.reconcileExpiredPlanMembers(root, scope.SessionID, teamID); err != nil {
		return nil, err
	}
	team, projection, err = s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return nil, err
	}
	out := make([]teams.Request, 0, len(projection.Requests))
	for _, item := range projection.Requests {
		if item.TeamID != teamID || (!actor.Lead && item.MemberID != actor.MemberID) {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ExpiresAt.Before(out[j].ExpiresAt)
	})
	if afterRequestID != "" {
		cursorIndex := -1
		for i := range out {
			if out[i].ID == afterRequestID {
				cursorIndex = i
				break
			}
		}
		if cursorIndex < 0 {
			return nil, teams.ErrNotFound
		}
		out = out[cursorIndex+1:]
	}
	pageSize := teams.PageSize(limit)
	if len(out) > pageSize {
		out = out[:pageSize]
	}
	return out, nil
}

func (s *Service) RespondTeamRequest(ctx context.Context, request agent.ExecutionRequest, teamID, requestID string, expectedRevision uint64, decision, feedback string) (teams.Request, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return teams.Request{}, err
	}
	feedback = redactRunCredential(feedback, s.deps.ProviderCredential)
	if teams.ValidateID(requestID) != nil || teams.ValidateText(feedback, teams.MaxFeedbackBytes, false) != nil {
		return teams.Request{}, errors.New("team request response is invalid")
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Request{}, err
	}
	if _, currentScope, currentActor, authErr := s.teamOperationScope(ctx, request); authErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Request{}, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Request{}, err
	}
	prior, ok := projection.Requests[requestID]
	if !ok || prior.TeamID != teamID {
		return prior, teams.ErrRevisionConflict
	}
	if prior.Revision != expectedRevision {
		// A retry of the exact response uses its original expected revision.
		// Only the persisted responder may receive the already-applied response
		// result, and it must match both the decision and sanitized feedback.
		if prior.Revision > expectedRevision && prior.Revision-expectedRevision == 1 && teamRequestResponder(actor, prior) && teamRequestAppliedDecision(prior.Status, decision) && prior.Feedback == feedback && (prior.Status != teams.RequestDeferred || time.Now().Before(prior.ExpiresAt)) {
			if prior.Type == teams.RequestPlan && prior.Status == teams.RequestApproved {
				member, ok := projection.Members[prior.MemberID]
				if !ok || member.TeamID != teamID {
					return teams.Request{}, teams.ErrNotFound
				}
				if !member.PlanApproved {
					member.PlanApproved = true
					if member.Status == teams.MemberAwaitingPlan {
						member.Status = teams.MemberIdle
					}
					member.Revision++
					if err := s.appendTeamMemberState(root, team, request.RunID, teams.Lead, member); err != nil {
						return teams.Request{}, err
					}
				}
				if s.teamScheduler != nil {
					s.teamScheduler.signalPlanResponse(request, scope, team.ID, member.ID, prior.ID)
				}
			}
			return prior, nil
		}
		return prior, teams.ErrRevisionConflict
	}
	if !teamRequestResponder(actor, prior) {
		return teams.Request{}, teams.ErrPermission
	}
	if (prior.Status == teams.RequestPending || prior.Status == teams.RequestDeferred) && !time.Now().Before(prior.ExpiresAt) {
		team, prior, err = s.expireTeamRequest(root, team, prior)
		if err != nil {
			return teams.Request{}, err
		}
		if prior.Type == teams.RequestPlan {
			if err := s.reconcileExpiredPlanMembers(root, scope.SessionID, teamID); err != nil {
				return prior, err
			}
		}
		return prior, errors.New("team request has expired")
	}
	if prior.Status != teams.RequestPending && prior.Status != teams.RequestDeferred {
		return prior, errors.New("team request is no longer answerable")
	}
	member := projection.Members[prior.MemberID]
	switch decision {
	case string(teams.RequestApproved):
		if prior.Type == teams.RequestShutdown && member.Status != teams.MemberIdle {
			return prior, errors.New("busy member shutdown must be handled at its next turn boundary")
		}
		prior.Status = teams.RequestApproved
	case string(teams.RequestRejected):
		prior.Status = teams.RequestRejected
	case string(teams.RequestDeferred):
		if prior.Type != teams.RequestShutdown || actor.Lead {
			return prior, teams.ErrPermission
		}
		prior.Status = teams.RequestDeferred
	default:
		return prior, errors.New("unsupported team request decision")
	}
	prior.Feedback = feedback
	prior.Revision++
	if err := s.appendTeamRequest(root, team, request.RunID, actorID(actor), sessionlog.TeamRequestResponded, prior); err != nil {
		return teams.Request{}, err
	}
	team.Revision++
	if prior.Type == teams.RequestPlan && prior.Status == teams.RequestApproved {
		member.PlanApproved = true
		if member.Status == teams.MemberAwaitingPlan {
			member.Status = teams.MemberIdle
		}
		member.Revision++
		if err := s.appendTeamMemberState(root, team, request.RunID, teams.Lead, member); err != nil {
			return prior, err
		}
		if actor.Lead && s.teamScheduler != nil {
			s.teamScheduler.signalPlanResponse(request, scope, team.ID, member.ID, prior.ID)
		}
	}
	if prior.Type == teams.RequestPlan && prior.Status == teams.RequestRejected && member.Status == teams.MemberAwaitingPlan {
		member.Status = teams.MemberIdle
		member.Revision++
		if err := s.appendTeamMemberState(root, team, request.RunID, teams.Lead, member); err != nil {
			return prior, err
		}
		if actor.Lead && s.teamScheduler != nil {
			s.teamScheduler.signalPlanResponse(request, scope, team.ID, member.ID, prior.ID)
		}
	}
	if prior.Type == teams.RequestShutdown && prior.Status == teams.RequestApproved {
		member.Status = teams.MemberStopped
		member.Revision++
		if err := s.appendTeamMemberState(root, team, request.RunID, actor.MemberID, member); err != nil {
			return prior, err
		}
	}
	return prior, nil
}

func teamRequestResponder(actor teams.Actor, request teams.Request) bool {
	if actorID(actor) != request.ResponderID {
		return false
	}
	if actor.Lead {
		return request.Type == teams.RequestPlan
	}
	return request.Type == teams.RequestShutdown && request.MemberID == actor.MemberID
}

func teamRequestAppliedDecision(status teams.RequestStatus, decision string) bool {
	switch status {
	case teams.RequestApproved:
		return decision == string(teams.RequestApproved)
	case teams.RequestRejected:
		return decision == string(teams.RequestRejected)
	case teams.RequestDeferred:
		return decision == string(teams.RequestDeferred)
	default:
		return false
	}
}

func (s *Service) createTeamRequest(root string, team teams.Team, runID, requester, memberID string, kind teams.RequestType, body string) (teams.Request, error) {
	return s.createTeamRequestUntil(root, team, runID, requester, memberID, kind, body, time.Now().UTC().Add(teams.RequestDuration))
}

func (s *Service) createTeamRequestUntil(root string, team teams.Team, runID, requester, memberID string, kind teams.RequestType, body string, expiresAt time.Time) (teams.Request, error) {
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Request{}, err
	}
	request := teams.Request{ID: id, TeamID: team.ID, MemberID: memberID, Type: kind, Status: teams.RequestPending, RequesterID: requester, Body: redactRunCredential(body, s.deps.ProviderCredential), ExpiresAt: expiresAt, Revision: 1}
	if kind == teams.RequestPlan {
		request.ResponderID = teams.Lead
	} else {
		request.ResponderID = memberID
	}
	if err := s.appendTeamRequest(root, team, runID, requester, sessionlog.TeamRequestCreated, request); err != nil {
		return teams.Request{}, err
	}
	return request, nil
}

func (s *Service) expireTeamRequest(root string, team teams.Team, request teams.Request) (teams.Team, teams.Request, error) {
	request.Status = teams.RequestExpired
	request.Revision++
	if err := s.appendTeamRequest(root, team, "", "service", sessionlog.TeamRequestExpired, request); err != nil {
		return team, teams.Request{}, err
	}
	team.Revision++
	return team, request, nil
}

// reconcileExpiredPlanMembers completes the second durable half of plan
// expiry. If expiry was recorded but the process stopped before updating an
// awaiting-plan member, a later list/response/resume retries this transition.
// Active turns are left alone; their terminal hook chooses idle after exit.
func (s *Service) reconcileExpiredPlanMembers(root, sessionID, teamID string) error {
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		return teams.ErrNotFound
	}
	now := time.Now().UTC()
	due := make([]teams.Request, 0)
	for _, request := range projection.Requests {
		if request.TeamID == teamID && request.Type == teams.RequestPlan && (request.Status == teams.RequestPending || request.Status == teams.RequestDeferred) && !now.Before(request.ExpiresAt) {
			due = append(due, request)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ExpiresAt.Equal(due[j].ExpiresAt) {
			return due[i].ID < due[j].ID
		}
		return due[i].ExpiresAt.Before(due[j].ExpiresAt)
	})
	for _, request := range due {
		if _, _, err := s.expireTeamRequest(root, team, request); err != nil {
			return err
		}
		projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
		if err != nil {
			return err
		}
		team = projection.Teams[teamID]
	}
	for _, member := range projection.Members {
		if member.TeamID != teamID || !member.PlanRequired || member.PlanApproved {
			continue
		}
		if member.Status != teams.MemberQueued && member.Status != teams.MemberRunning && member.Status != teams.MemberStopping && member.Status != teams.MemberAwaitingPlan {
			continue
		}
		turn, hasTurn := projection.Turns[member.TurnID]
		if !hasTurn || !turnTerminalTeamStatus(turn.Status) {
			continue
		}
		planExpired, pendingPlan := false, false
		for _, request := range projection.Requests {
			if request.TeamID != teamID || request.MemberID != member.ID || request.Type != teams.RequestPlan {
				continue
			}
			switch request.Status {
			case teams.RequestExpired:
				planExpired = true
			case teams.RequestPending, teams.RequestDeferred:
				pendingPlan = true
			}
		}
		if member.Status == teams.MemberAwaitingPlan {
			if planExpired && !pendingPlan {
				member.Status = teams.MemberIdle
				member.Revision++
				if err := appendTeamFactLocked(root, team.Scope.SessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
					return err
				}
				projection.Members[member.ID] = member
			}
			continue
		}
		target := teams.MemberInterrupted
		if turn.Status == "succeeded" {
			target = teams.MemberIdle
		}
		if member.Status == teams.MemberStopping {
			target = teams.MemberStopped
		} else if target == teams.MemberIdle && planExpired && !pendingPlan {
			target = teams.MemberIdle
		} else if target == teams.MemberIdle {
			target = teams.MemberAwaitingPlan
		}
		if member.Budget.CanAccept() != nil && target != teams.MemberStopped {
			target = teams.MemberBudgetExhausted
		}
		if member.Status == target {
			continue
		}
		member.Status = target
		member.Revision++
		if err := appendTeamFactLocked(root, team.Scope.SessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
			return err
		}
		projection.Members[member.ID] = member
	}
	return nil
}

// expireDueTeamRequestsForMember is called at turn completion so a request
// that reached its deadline while the child was active is made durable before
// the member's terminal state is selected.
func (s *Service) expireDueTeamRequestsForMember(root, sessionID, teamID, memberID string) error {
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		return teams.ErrNotFound
	}
	now := time.Now().UTC()
	due := make([]teams.Request, 0)
	for _, request := range projection.Requests {
		if request.TeamID == teamID && request.MemberID == memberID && (request.Status == teams.RequestPending || request.Status == teams.RequestDeferred) && !now.Before(request.ExpiresAt) {
			due = append(due, request)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ExpiresAt.Equal(due[j].ExpiresAt) {
			return due[i].ID < due[j].ID
		}
		return due[i].ExpiresAt.Before(due[j].ExpiresAt)
	})
	for _, request := range due {
		team, _, err = s.expireTeamRequest(root, team, request)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) expireDueTeamRequestsForTeam(root, sessionID, teamID string) error {
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		return teams.ErrNotFound
	}
	now := time.Now().UTC()
	due := make([]teams.Request, 0)
	for _, request := range projection.Requests {
		if request.TeamID == teamID && (request.Status == teams.RequestPending || request.Status == teams.RequestDeferred) && !now.Before(request.ExpiresAt) {
			due = append(due, request)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ExpiresAt.Equal(due[j].ExpiresAt) {
			return due[i].ID < due[j].ID
		}
		return due[i].ExpiresAt.Before(due[j].ExpiresAt)
	})
	for _, request := range due {
		team, _, err = s.expireTeamRequest(root, team, request)
		if err != nil {
			return err
		}
	}
	return s.reconcileExpiredPlanMembers(root, sessionID, teamID)
}

func (s *Service) appendTeamRequest(root string, team teams.Team, runID, actor, kind string, request teams.Request) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	_, err = sessionlog.Append(root, team.Scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: team.ID, SessionID: team.Scope.SessionID, Kind: kind, Revision: team.Revision + 1, ActorID: actor, ActorRunID: runID, Request: &request})
	if err == nil && s.teamScheduler != nil {
		s.teamScheduler.trackExpiryScope(root, team.Scope.SessionID, team.ID)
	}
	return err
}

func (s *Service) appendTeamMemberState(root string, team teams.Team, runID, actor string, member teams.Member) error {
	if s.teamMemberStateAppender != nil {
		return s.teamMemberStateAppender(root, team, runID, actor, member)
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	_, err = sessionlog.Append(root, team.Scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: team.ID, SessionID: team.Scope.SessionID, Kind: sessionlog.TeamMemberState, Revision: team.Revision + 1, ActorID: actor, ActorRunID: runID, Member: &member})
	return err
}
