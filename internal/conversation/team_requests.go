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

func (s *Service) ListTeamRequests(ctx context.Context, request agent.ExecutionRequest, teamID string) ([]teams.Request, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return nil, err
	}
	_, projection, err := s.teamForOperation(root, scope, teamID, actor)
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
	limit := teams.PageSize(0)
	if len(out) > limit {
		out = out[:limit]
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
	if !ok || prior.TeamID != teamID || prior.Revision != expectedRevision {
		return prior, teams.ErrRevisionConflict
	}
	if actor.Lead && prior.Type != teams.RequestPlan || !actor.Lead && (prior.Type != teams.RequestShutdown || prior.MemberID != actor.MemberID) {
		return teams.Request{}, teams.ErrPermission
	}
	if prior.Status != teams.RequestPending && prior.Status != teams.RequestDeferred || !time.Now().Before(prior.ExpiresAt) {
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
			s.teamScheduler.signalFromLead(request, scope, team.ID, member.ID, prior.ID)
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

func (s *Service) createTeamRequest(root string, team teams.Team, runID, requester, memberID string, kind teams.RequestType, body string) (teams.Request, error) {
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Request{}, err
	}
	request := teams.Request{ID: id, TeamID: team.ID, MemberID: memberID, Type: kind, Status: teams.RequestPending, RequesterID: requester, Body: redactRunCredential(body, s.deps.ProviderCredential), ExpiresAt: time.Now().UTC().Add(teams.RequestDuration), Revision: 1}
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

func (s *Service) appendTeamRequest(root string, team teams.Team, runID, actor, kind string, request teams.Request) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	_, err = sessionlog.Append(root, team.Scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: team.ID, SessionID: team.Scope.SessionID, Kind: kind, Revision: team.Revision + 1, ActorID: actor, ActorRunID: runID, Request: &request})
	return err
}

func (s *Service) appendTeamMemberState(root string, team teams.Team, runID, actor string, member teams.Member) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	_, err = sessionlog.Append(root, team.Scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: team.ID, SessionID: team.Scope.SessionID, Kind: sessionlog.TeamMemberState, Revision: team.Revision + 1, ActorID: actor, ActorRunID: runID, Member: &member})
	return err
}
