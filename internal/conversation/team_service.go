package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// teamScope resolves a run's durable attribution and the currently authorized
// project root. It deliberately does not call BuildAuthority: team creation
// must remain available after the run has created its candidate directory.
func (s *Service) teamScope(ctx context.Context, request agent.ExecutionRequest) (string, teams.Scope, error) {
	var zero teams.Scope
	if err := ctx.Err(); err != nil {
		return "", zero, err
	}
	if request.TeamTurn != nil || request.TeamUser {
		return "", zero, teams.ErrPermission
	}
	root, err := sessionlog.ProjectRoot(s.deps.ProjectRoot)
	if err != nil {
		return "", zero, err
	}
	if err = sessionlog.ValidateID(request.Work.SessionID); err != nil {
		return "", zero, err
	}
	persisted, found, err := sessionlog.FindRunStart(root, request.Work.SessionID, request.RunID)
	if err != nil {
		return "", zero, err
	}
	if !found || persisted.TeamID != "" || !workRefMatchesRun(request.Work, persisted) {
		return "", zero, teams.ErrPermission
	}
	return s.scopeForWork(ctx, root, request.Work)
}

func workRefMatchesRun(work agent.WorkRef, run sessionlog.RunStarted) bool {
	return work.SessionID != "" && string(work.Kind) == run.WorkKind && work.GoalID == run.GoalID && work.WorkItemID == run.WorkItemID
}

func persistedRunWork(root, sessionID, runID string) (agent.WorkRef, error) {
	run, found, err := sessionlog.FindRunStart(root, sessionID, runID)
	if err != nil {
		return agent.WorkRef{}, err
	}
	if found {
		return agent.WorkRef{Kind: agent.WorkKind(run.WorkKind), SessionID: sessionID, GoalID: run.GoalID, WorkItemID: run.WorkItemID}, nil
	}
	return agent.WorkRef{}, teams.ErrPermission
}

func (s *Service) scopeForWork(ctx context.Context, projectRoot string, work agent.WorkRef) (string, teams.Scope, error) {
	scopeRoot := projectRoot
	switch work.Kind {
	case agent.WorkSession:
		if work.GoalID != "" || work.WorkItemID != "" {
			return "", teams.Scope{}, teams.ErrPermission
		}
	case agent.WorkGoal:
		if s.deps.Store == nil {
			return "", teams.Scope{}, errors.New("team goal authorization store is unavailable")
		}
		if !validComponent(work.GoalID) || !validComponent(work.WorkItemID) {
			return "", teams.Scope{}, teams.ErrPermission
		}
		goal, err := s.deps.Store.GetGoalSnapshot(ctx, work.GoalID)
		if err != nil || goal.Goal.SourceSessionID != work.SessionID {
			return "", teams.Scope{}, teams.ErrPermission
		}
		scopeRoot, err = filepath.EvalSymlinks(goal.Goal.AllowedRoot)
		if err != nil {
			return "", teams.Scope{}, err
		}
	default:
		return "", teams.Scope{}, teams.ErrPermission
	}
	scopeRoot, err := filepath.Abs(scopeRoot)
	if err != nil {
		return "", teams.Scope{}, err
	}
	scope := teams.Scope{SessionID: work.SessionID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, ProjectRoot: filepath.Clean(scopeRoot), ProviderName: s.deps.ProviderName}
	if err := scope.Validate(); err != nil {
		return "", teams.Scope{}, err
	}
	return projectRoot, scope, nil
}

func (s *Service) activeLeadRun(request agent.ExecutionRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeRuns != nil && s.activeRuns[request.RunID] == request.Work.SessionID
}

func (s *Service) activeRunRequest(sessionID, runID string) (agent.ExecutionRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request, ok := s.activeRequests[runID]
	if !ok || s.activeRuns[runID] != sessionID || request.Work.SessionID != sessionID || request.RunID != runID || request.TeamTurn != nil || request.TeamUser {
		return agent.ExecutionRequest{}, teams.ErrPermission
	}
	return request, nil
}

// teamOperationScope derives the actor from the persisted parent run or the
// exact accepted member turn. Sender and work scope never come from operation
// arguments.
func (s *Service) teamOperationScope(ctx context.Context, request agent.ExecutionRequest) (string, teams.Scope, teams.Actor, error) {
	var zero teams.Scope
	if err := ctx.Err(); err != nil {
		return "", zero, teams.Actor{}, err
	}
	root, err := sessionlog.ProjectRoot(s.deps.ProjectRoot)
	if err != nil {
		return "", zero, teams.Actor{}, err
	}
	if request.TeamUser {
		if request.TeamTurn != nil || sessionlog.ValidateID(request.Work.SessionID) != nil {
			return "", zero, teams.Actor{}, teams.ErrPermission
		}
		_, scope, scopeErr := s.scopeForWork(ctx, root, request.Work)
		if scopeErr != nil {
			return "", zero, teams.Actor{}, teams.ErrPermission
		}
		return root, scope, teams.Actor{Lead: true}, nil
	}
	if sessionlog.ValidateID(request.Work.SessionID) != nil {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	// Parent lead tools are authorized by the in-memory active parent run.
	// Member tools are children in the shared pool and have their own durable
	// RunStarted/TeamTurn facts; requiring activeLeadRun here incorrectly ties
	// their authority to the parent having remained active after spawn.
	if request.TeamTurn == nil && !s.activeLeadRun(request) {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	run, found, err := sessionlog.FindRunStart(root, request.Work.SessionID, request.RunID)
	if err != nil {
		return "", zero, teams.Actor{}, err
	}
	if !found || !workRefMatchesRun(request.Work, run) {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	if request.TeamTurn == nil {
		if run.TeamID != "" {
			return "", zero, teams.Actor{}, teams.ErrPermission
		}
		_, scope, scopeErr := s.scopeForWork(ctx, root, request.Work)
		return root, scope, teams.Actor{Lead: true}, scopeErr
	}
	identity := *request.TeamTurn
	if identity.Validate() != nil || run.TeamID != identity.TeamID || run.TeamMemberID != identity.MemberID || run.TeamTurnID != identity.TurnID {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, identity.TeamID)
	if err != nil {
		return "", zero, teams.Actor{}, err
	}
	team, ok := projection.Teams[identity.TeamID]
	member, memberOK := projection.Members[identity.MemberID]
	if !ok || !memberOK || team.Status != teams.TeamOpen || member.TeamID != team.ID || member.RunID != request.RunID || member.TurnID != identity.TurnID || !member.Status.HasTurn() {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	_, current, err := s.scopeForWork(ctx, root, request.Work)
	if err != nil || !current.Matches(team.Scope) {
		return "", zero, teams.Actor{}, teams.ErrPermission
	}
	return root, team.Scope, teams.Actor{MemberID: identity.MemberID}, nil
}

// teamUserRequest binds a local UI action to an existing team and its
// currently authorized WorkRef. The client supplies only session/team IDs;
// root, goal/work item and lead identity are rebuilt from service facts.
func (s *Service) teamUserRequest(ctx context.Context, sessionID, teamID string) (agent.ExecutionRequest, error) {
	var request agent.ExecutionRequest
	if sessionlog.ValidateID(sessionID) != nil || teams.ValidateID(teamID) != nil {
		return request, teams.ErrPermission
	}
	root, err := sessionlog.ProjectRoot(s.deps.ProjectRoot)
	if err != nil {
		return request, err
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return request, err
	}
	team, ok := projection.Teams[teamID]
	if !ok || team.Scope.SessionID != sessionID || !s.teamScopeStillAuthorized(ctx, root, team) {
		return request, teams.ErrPermission
	}
	work := agent.WorkRef{Kind: agent.WorkKind(team.Scope.WorkKind), SessionID: sessionID, GoalID: team.Scope.GoalID, WorkItemID: team.Scope.WorkItemID}
	_, scope, err := s.scopeForWork(ctx, root, work)
	if err != nil || !scope.Matches(team.Scope) {
		return request, teams.ErrPermission
	}
	return agent.ExecutionRequest{Work: work, TeamUser: true}, nil
}

func (s *Service) CreateTeam(ctx context.Context, request agent.ExecutionRequest, name string) (teams.Team, error) {
	root, scope, err := s.teamScope(ctx, request)
	if err != nil {
		return teams.Team{}, err
	}
	if !s.activeLeadRun(request) {
		return teams.Team{}, teams.ErrPermission
	}
	name, err = teams.NormalizeName(name)
	if err != nil {
		return teams.Team{}, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err = ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	if !s.activeLeadRun(request) {
		return teams.Team{}, teams.ErrPermission
	}
	projection, err := sessionlog.ReplayTeams(root, scope.SessionID)
	if err != nil {
		return teams.Team{}, err
	}
	sessionTeams, sessionMembers := teamCapacity(projection)
	serviceTeams, serviceMembers, err := serviceTeamCapacity(root)
	if err != nil {
		return teams.Team{}, err
	}
	if err = teams.CheckCapacity(sessionTeams, serviceTeams, sessionMembers, serviceMembers, true, false); err != nil {
		return teams.Team{}, err
	}
	for _, prior := range projection.Teams {
		if prior.Status != teams.TeamClosed && prior.Name == name {
			return teams.Team{}, errors.New("team name is already active in this session")
		}
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Team{}, err
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		return teams.Team{}, err
	}
	team := teams.Team{ID: id, Name: name, Scope: scope, CreatorRunID: request.RunID, Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC()}
	if err := ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	_, err = sessionlog.Append(root, scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: eventID, TeamID: id, SessionID: scope.SessionID, Kind: sessionlog.TeamCreated, Revision: 1, ActorID: teams.Lead, ActorRunID: request.RunID, Team: &team})
	return team, err
}

func teamCapacity(projection sessionlog.TeamProjection) (int, int) {
	openTeams, members := 0, 0
	for _, team := range projection.Teams {
		if team.Status != teams.TeamClosed {
			openTeams++
		}
	}
	for _, member := range projection.Members {
		if !member.Status.IsTerminal() {
			members++
		}
	}
	return openTeams, members
}

func serviceTeamCapacity(root string) (int, int, error) {
	dir, err := sessionlog.SessionDir(root)
	if err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	openTeams, members := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) != 38 || name[32:] != ".jsonl" {
			continue
		}
		sessionID := name[:32]
		if sessionlog.ValidateID(sessionID) != nil {
			continue
		}
		projection, replayErr := sessionlog.ReplayTeams(root, sessionID)
		if replayErr != nil {
			return 0, 0, replayErr
		}
		teamCount, count := teamCapacity(projection)
		openTeams += teamCount
		members += count
	}
	return openTeams, members, nil
}

func (s *Service) ListTeams(ctx context.Context, request agent.ExecutionRequest, requestedLimit ...int) ([]teams.Team, error) {
	if len(requestedLimit) > 1 {
		return nil, errors.New("team query accepts at most one limit")
	}
	limit := 0
	if len(requestedLimit) == 1 {
		limit = requestedLimit[0]
	}
	return s.ListTeamsPage(ctx, request, "", limit)
}

// ListTeamsPage returns a bounded created-time/ID-ordered page after a visible team ID.
func (s *Service) ListTeamsPage(ctx context.Context, request agent.ExecutionRequest, afterTeamID string, limit int) ([]teams.Team, error) {
	if afterTeamID != "" && teams.ValidateID(afterTeamID) != nil {
		return nil, errors.New("team list cursor is invalid")
	}
	root, _, err := s.scopeForWork(ctx, currentProjectRoot(s.deps.ProjectRoot), request.Work)
	if err != nil {
		return nil, err
	}
	list, err := s.listTeamsForSession(ctx, root, request.Work.SessionID, &request.Work)
	if err != nil {
		return nil, err
	}
	return pageTeams(list, afterTeamID, limit)
}

func pageTeams(list []teams.Team, afterTeamID string, limit int) ([]teams.Team, error) {
	if afterTeamID != "" {
		cursorIndex := -1
		for i := range list {
			if list[i].ID == afterTeamID {
				cursorIndex = i
				break
			}
		}
		if cursorIndex < 0 {
			return nil, teams.ErrNotFound
		}
		list = list[cursorIndex+1:]
	}
	pageSize := teams.PageSize(limit)
	if len(list) > pageSize {
		list = list[:pageSize]
	}
	return list, nil
}

func currentProjectRoot(path string) string {
	root, err := sessionlog.ProjectRoot(path)
	if err != nil {
		return path
	}
	return root
}

func (s *Service) listTeamsForSession(ctx context.Context, root, sessionID string, work *agent.WorkRef) ([]teams.Team, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, err := sessionlog.TeamIDs(root, sessionID)
	if err != nil {
		return nil, err
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, ids...)
	if err != nil {
		return nil, err
	}
	var out []teams.Team
	for _, team := range projection.Teams {
		if !s.teamScopeStillAuthorized(ctx, root, team) {
			return nil, teams.ErrPermission
		}
		if work != nil && (team.Scope.WorkKind != string(work.Kind) || team.Scope.GoalID != work.GoalID || team.Scope.WorkItemID != work.WorkItemID) {
			continue
		}
		out = append(out, team)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Service) teamScopeStillAuthorized(ctx context.Context, projectRoot string, team teams.Team) bool {
	root, scope, err := s.scopeForWork(ctx, projectRoot, agent.WorkRef{Kind: agent.WorkKind(team.Scope.WorkKind), SessionID: team.Scope.SessionID, GoalID: team.Scope.GoalID, WorkItemID: team.Scope.WorkItemID})
	return err == nil && root == projectRoot && scope.Matches(team.Scope)
}

func (s *Service) GetTeam(ctx context.Context, request agent.ExecutionRequest, teamID string) (teams.Team, error) {
	root, _, err := s.scopeForWork(ctx, currentProjectRoot(s.deps.ProjectRoot), request.Work)
	if err != nil {
		return teams.Team{}, err
	}
	return s.getTeamForSession(ctx, root, request.Work.SessionID, teamID, &request.Work)
}

func (s *Service) getTeamForSession(ctx context.Context, root, sessionID, teamID string, work *agent.WorkRef) (teams.Team, error) {
	if err := ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	if teams.ValidateID(teamID) != nil {
		return teams.Team{}, teams.ErrNotFound
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return teams.Team{}, err
	}
	team := projection.Teams[teamID]
	if !s.teamScopeStillAuthorized(ctx, root, team) || team.Scope.SessionID != sessionID || (work != nil && (team.Scope.WorkKind != string(work.Kind) || team.Scope.GoalID != work.GoalID || team.Scope.WorkItemID != work.WorkItemID)) {
		return teams.Team{}, teams.ErrPermission
	}
	return team, nil
}

func (s *Service) CloseTeam(ctx context.Context, request agent.ExecutionRequest, teamID string) (teams.Team, error) {
	root, _, err := s.scopeForWork(ctx, currentProjectRoot(s.deps.ProjectRoot), request.Work)
	if err != nil {
		return teams.Team{}, err
	}
	team, err := s.closeTeamForSession(ctx, root, request.Work.SessionID, teamID, &request.Work)
	if err == nil && s.teamScheduler != nil {
		for _, cancel := range s.teamScheduler.invalidateTeam(teamID) {
			cancel()
		}
	}
	return team, err
}

func (s *Service) closeTeamForSession(ctx context.Context, root, sessionID, teamID string, work *agent.WorkRef) (teams.Team, error) {
	if err := ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	team, err := s.getTeamForSession(ctx, root, sessionID, teamID, work)
	if err != nil {
		return teams.Team{}, err
	}
	if team.Status == teams.TeamClosed || team.Status == teams.TeamClosing {
		return team, nil
	}
	next := team
	next.Status = teams.TeamClosing
	next.Revision++
	if err := ctx.Err(); err != nil {
		return teams.Team{}, err
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Team{}, err
	}
	if _, err = sessionlog.Append(root, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamClosing, Revision: next.Revision, ActorID: "service", Team: &next}); err != nil {
		return teams.Team{}, err
	}
	return s.closeTeamIfIdleLocked(root, sessionID, teamID)
}

// closeTeamIfIdleLocked stops members that have no accepted turn and writes
// TeamClosed only after every active child has reached a durable turn terminal.
// The caller holds eventMu, so the projection and transitions are serialized.
func (s *Service) closeTeamIfIdleLocked(root, sessionID, teamID string) (teams.Team, error) {
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return teams.Team{}, err
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		return teams.Team{}, teams.ErrNotFound
	}
	if team.Status == teams.TeamClosed {
		return team, nil
	}
	if team.Status != teams.TeamClosing {
		return team, nil
	}
	active := false
	for _, member := range projection.Members {
		if member.TeamID != teamID || member.Status.IsTerminal() {
			continue
		}
		if member.Status.HasTurn() {
			active = true
			continue
		}
		member.Status = teams.MemberStopped
		member.Revision++
		if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
			return team, err
		}
	}
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return teams.Team{}, err
	}
	team = projection.Teams[teamID]
	for _, member := range projection.Members {
		if member.TeamID == teamID && member.Status.HasTurn() {
			active = true
		}
	}
	if active {
		return team, nil
	}
	team.Status = teams.TeamClosed
	team.Revision++
	id, err := sessionlog.NewID()
	if err != nil {
		return team, err
	}
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: id, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamClosed, Revision: team.Revision, ActorID: "service", Team: &team})
	return team, err
}

func (s *Service) handleTeamRequest(ctx context.Context, msg ClientMsg) (ServerMsg, error) {
	root, err := sessionlog.ProjectRoot(s.deps.ProjectRoot)
	if err != nil {
		return ServerMsg{}, err
	}
	request := agent.ExecutionRequest{RunID: msg.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: msg.SessionID}}
	switch msg.Op {
	case "team_coordinator":
		if _, err := sessionlog.SessionPath(root, msg.SessionID); err != nil {
			return ServerMsg{}, err
		}
		if err := ctx.Err(); err != nil {
			return ServerMsg{}, err
		}
		s.eventMu.Lock()
		_, err = sessionlog.Append(root, msg.SessionID, sessionlog.EventCoordinatorMode, sessionlog.CoordinatorMode{Enabled: msg.CoordinatorOn})
		s.eventMu.Unlock()
		if err != nil {
			return ServerMsg{}, err
		}
		return ServerMsg{Type: msg.Op, CoordinatorOn: msg.CoordinatorOn}, nil
	case "team_create":
		request.Work, err = persistedRunWork(root, msg.SessionID, msg.RunID)
		if err != nil {
			return ServerMsg{}, err
		}
		team, createErr := s.CreateTeam(ctx, request, msg.TeamName)
		return ServerMsg{Type: msg.Op, Team: &team}, createErr
	case "team_list":
		list, listErr := s.listTeamsForSession(ctx, root, msg.SessionID, nil)
		if listErr == nil {
			list, listErr = pageTeams(list, msg.AfterTeamID, msg.Limit)
		}
		return ServerMsg{Type: msg.Op, Teams: list}, listErr
	case "team_get":
		team, getErr := s.getTeamForSession(ctx, root, msg.SessionID, msg.TeamID, nil)
		return ServerMsg{Type: msg.Op, Team: &team}, getErr
	case "team_close":
		team, closeErr := s.closeTeamForSession(ctx, root, msg.SessionID, msg.TeamID, nil)
		if closeErr == nil && s.teamScheduler != nil {
			for _, cancel := range s.teamScheduler.invalidateTeam(msg.TeamID) {
				cancel()
			}
		}
		return ServerMsg{Type: msg.Op, Team: &team}, closeErr
	case "team_member_spawn", "team_member_resume":
		request, err = s.activeRunRequest(msg.SessionID, msg.RunID)
		if err != nil {
			return ServerMsg{}, err
		}
		work, workErr := persistedRunWork(root, msg.SessionID, msg.RunID)
		if workErr != nil || work != request.Work {
			return ServerMsg{}, teams.ErrPermission
		}
		callID, idErr := sessionlog.NewID()
		if idErr != nil {
			return ServerMsg{}, idErr
		}
		if msg.Op == "team_member_spawn" {
			member, spawnErr := s.SpawnTeamMember(ctx, request, TeamMemberSpawnRequest{TeamID: msg.TeamID, Name: msg.TeamMemberName, AgentName: msg.AgentName, Instruction: msg.Text, PlanRequired: msg.TeamPlanRequired, OriginCallID: callID})
			return ServerMsg{Type: msg.Op, TeamMember: &member}, spawnErr
		}
		request.AcceptTeamRoleChange = msg.TeamAcceptRoleChange
		member, resumeErr := s.ResumeTeamMember(ctx, request, msg.TeamID, msg.TeamMemberID, callID)
		return ServerMsg{Type: msg.Op, TeamMember: &member}, resumeErr
	case "team_member_stop":
		member, stopErr := s.StopTeamMember(ctx, msg.SessionID, msg.TeamID, msg.TeamMemberID)
		return ServerMsg{Type: msg.Op, TeamMember: &member}, stopErr
	case "team_send", "team_messages":
		if msg.RunID == "" {
			request, err = s.teamUserRequest(ctx, msg.SessionID, msg.TeamID)
		} else if msg.Op == "team_send" {
			request, err = s.activeRunRequest(msg.SessionID, msg.RunID)
			if err == nil {
				var work agent.WorkRef
				work, err = persistedRunWork(root, msg.SessionID, msg.RunID)
				if err == nil && work != request.Work {
					err = teams.ErrPermission
				}
			}
		} else {
			request.Work, err = persistedRunWork(root, msg.SessionID, msg.RunID)
		}
		if err != nil {
			return ServerMsg{}, err
		}
		if msg.Op == "team_send" {
			message, sendErr := s.SendTeamMessage(ctx, request, TeamSendRequest{TeamID: msg.TeamID, Recipient: msg.TeamRecipient, Body: msg.Text, Token: msg.TeamToken, Broadcast: msg.TeamBroadcast})
			return ServerMsg{Type: msg.Op, TeamMessage: &message}, sendErr
		}
		messages, listErr := s.ListTeamMessages(ctx, request, msg.TeamID, msg.AfterSeq, msg.Limit)
		return ServerMsg{Type: msg.Op, TeamMessages: messages}, listErr
	case "team_request_list", "team_request_respond", "team_shutdown_request":
		request, err = s.teamUserRequest(ctx, msg.SessionID, msg.TeamID)
		if err != nil {
			return ServerMsg{}, err
		}
		switch msg.Op {
		case "team_request_list":
			requests, requestErr := s.ListTeamRequestsPage(ctx, request, msg.TeamID, msg.AfterTeamRequestID, msg.Limit)
			return ServerMsg{Type: msg.Op, TeamRequests: requests}, requestErr
		case "team_request_respond":
			requestFact, requestErr := s.RespondTeamRequest(ctx, request, msg.TeamID, msg.TeamRequestID, msg.ExpectedRevision, msg.TeamDecision, msg.TeamFeedback)
			return ServerMsg{Type: msg.Op, TeamRequest: &requestFact}, requestErr
		default:
			requestFact, requestErr := s.RequestTeamShutdown(ctx, request, msg.TeamID, msg.TeamMemberID)
			return ServerMsg{Type: msg.Op, TeamRequest: &requestFact}, requestErr
		}
	case "team_task_create", "team_task_get", "team_task_list", "team_task_update":
		if msg.RunID == "" {
			request, err = s.teamUserRequest(ctx, msg.SessionID, msg.TeamID)
		} else {
			request.Work, err = persistedRunWork(root, msg.SessionID, msg.RunID)
		}
		if err != nil {
			return ServerMsg{}, err
		}
		switch msg.Op {
		case "team_task_create":
			description := ""
			if msg.TaskDescription != nil {
				description = *msg.TaskDescription
			}
			input := teams.Task{Title: *msg.TaskTitle, Description: description}
			if msg.TaskAssignee != nil {
				input.Assignee = *msg.TaskAssignee
			}
			if msg.TaskBlockedBy != nil {
				input.BlockedBy = *msg.TaskBlockedBy
			}
			task, taskErr := s.CreateTeamTask(ctx, request, msg.TeamID, input)
			return ServerMsg{Type: msg.Op, TeamTask: &task}, taskErr
		case "team_task_get":
			task, taskErr := s.GetTeamTask(ctx, request, msg.TeamID, msg.TaskID)
			return ServerMsg{Type: msg.Op, TeamTask: &task}, taskErr
		case "team_task_list":
			tasks, taskErr := s.ListTeamTasksPage(ctx, request, msg.TeamID, msg.AfterTaskID, msg.Limit)
			return ServerMsg{Type: msg.Op, TeamTasks: tasks}, taskErr
		case "team_task_update":
			patch := teams.TaskPatch{Title: msg.TaskTitle, Description: msg.TaskDescription, Assignee: msg.TaskAssignee, BlockedBy: msg.TaskBlockedBy}
			if msg.TaskStatus != nil {
				status := teams.TaskStatus(*msg.TaskStatus)
				patch.Status = &status
			}
			task, taskErr := s.UpdateTeamTask(ctx, request, msg.TeamID, msg.TaskID, msg.ExpectedRevision, patch)
			return ServerMsg{Type: msg.Op, TeamTask: &task}, taskErr
		}
		return ServerMsg{}, errors.New("unknown team task operation")
	default:
		return ServerMsg{}, errors.New("unknown team operation")
	}
}

func teamCoordinatorModeEnabled(root, sessionID string) (bool, error) {
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return false, err
	}
	enabled := false
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventCoordinatorMode {
			continue
		}
		var mode sessionlog.CoordinatorMode
		if decodeSessionData(event.Data, &mode) != nil {
			return false, errors.New("coordinator mode history is invalid")
		}
		enabled = mode.Enabled
	}
	return enabled, nil
}
