package conversation

import (
	"context"
	"errors"
	"sort"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// CreateTeamTask creates one team-local task. Identity, team ID, creator and
// revision are assigned by the service; only task content is accepted.
func (s *Service) CreateTeamTask(ctx context.Context, request agent.ExecutionRequest, teamID string, input teams.Task) (teams.Task, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return teams.Task{}, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Task{}, err
	}
	if _, currentScope, currentActor, scopeErr := s.teamOperationScope(ctx, request); scopeErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Task{}, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Task{}, err
	}
	if input.Assignee != "" {
		member, ok := projection.Members[input.Assignee]
		if !ok || member.TeamID != teamID {
			return teams.Task{}, teams.ErrPermission
		}
	}
	graph, err := teamTaskGraph(projection, teamID)
	if err != nil {
		return teams.Task{}, err
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Task{}, err
	}
	input.ID, input.TeamID, input.CreatedBy, input.Revision = id, teamID, "", 0
	input.Status = teams.TaskPending
	task, err := graph.Create(input, actor)
	if err != nil {
		return teams.Task{}, err
	}
	persistedTask := task
	persistedTask.Status = teams.TaskPending
	eventID, err := sessionlog.NewID()
	if err != nil {
		return teams.Task{}, err
	}
	if err := ctx.Err(); err != nil {
		return teams.Task{}, err
	}
	_, err = sessionlog.Append(root, scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: eventID, TeamID: teamID, SessionID: scope.SessionID, Kind: sessionlog.TeamTaskCreated, Revision: team.Revision + 1, ActorID: task.CreatedBy, ActorRunID: request.RunID, Task: &persistedTask})
	if err == nil && actor.Lead && task.Assignee != "" && s.teamScheduler != nil {
		s.teamScheduler.signalFromLead(request, scope, teamID, task.Assignee, "task:"+task.ID)
	}
	return task, err
}

func (s *Service) GetTeamTask(ctx context.Context, request agent.ExecutionRequest, teamID, taskID string) (teams.Task, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return teams.Task{}, err
	}
	_, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Task{}, err
	}
	task, ok := projection.Tasks[taskID]
	if !ok || task.TeamID != teamID {
		return teams.Task{}, teams.ErrNotFound
	}
	graph, err := teamTaskGraph(projection, teamID)
	if err != nil {
		return teams.Task{}, err
	}
	projected, ok := graph.Get(taskID)
	if !ok {
		return teams.Task{}, teams.ErrNotFound
	}
	return projected, nil
}

func (s *Service) ListTeamTasks(ctx context.Context, request agent.ExecutionRequest, teamID string, requestedLimit ...int) ([]teams.Task, error) {
	if len(requestedLimit) > 1 {
		return nil, errors.New("team task query accepts at most one limit")
	}
	limit := 0
	if len(requestedLimit) == 1 {
		limit = requestedLimit[0]
	}
	return s.ListTeamTasksPage(ctx, request, teamID, "", limit)
}

// ListTeamTasksPage returns a bounded task-ID-ordered page after afterTaskID.
func (s *Service) ListTeamTasksPage(ctx context.Context, request agent.ExecutionRequest, teamID, afterTaskID string, limit int) ([]teams.Task, error) {
	if afterTaskID != "" && teams.ValidateID(afterTaskID) != nil {
		return nil, errors.New("team task cursor is invalid")
	}
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return nil, err
	}
	_, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return nil, err
	}
	graph, err := teamTaskGraph(projection, teamID)
	if err != nil {
		return nil, err
	}
	tasks := graph.List()
	if afterTaskID != "" {
		start := sort.Search(len(tasks), func(i int) bool { return tasks[i].ID > afterTaskID })
		tasks = tasks[start:]
	}
	pageSize := teams.PageSize(limit)
	if len(tasks) > pageSize {
		tasks = tasks[:pageSize]
	}
	return tasks, nil
}

func (s *Service) UpdateTeamTask(ctx context.Context, request agent.ExecutionRequest, teamID, taskID string, expectedRevision uint64, patch teams.TaskPatch) (teams.Task, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return teams.Task{}, err
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err := ctx.Err(); err != nil {
		return teams.Task{}, err
	}
	if _, currentScope, currentActor, scopeErr := s.teamOperationScope(ctx, request); scopeErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Task{}, teams.ErrPermission
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Task{}, err
	}
	if patch.Assignee != nil && *patch.Assignee != "" {
		member, ok := projection.Members[*patch.Assignee]
		if !ok || member.TeamID != teamID {
			return teams.Task{}, teams.ErrPermission
		}
	}
	graph, err := teamTaskGraph(projection, teamID)
	if err != nil {
		return teams.Task{}, err
	}
	task, err := graph.Update(taskID, expectedRevision, patch, actor)
	if err != nil {
		return task, err
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		return teams.Task{}, err
	}
	if err := ctx.Err(); err != nil {
		return teams.Task{}, err
	}
	persistedTask, ok := graph.GetCanonical(taskID)
	if !ok {
		return teams.Task{}, teams.ErrNotFound
	}
	persistedTask.Blocks = nil
	_, err = sessionlog.Append(root, scope.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: eventID, TeamID: teamID, SessionID: scope.SessionID, Kind: sessionlog.TeamTaskUpdated, Revision: team.Revision + 1, ActorID: actorID(actor), ActorRunID: request.RunID, Task: &persistedTask})
	if err == nil && actor.Lead && task.Assignee != "" && task.Status != teams.TaskCompleted && s.teamScheduler != nil {
		s.teamScheduler.signalFromLead(request, scope, teamID, task.Assignee, "task:"+task.ID)
	}
	return task, err
}

func actorID(actor teams.Actor) string {
	if actor.Lead {
		return teams.Lead
	}
	return actor.MemberID
}

func (s *Service) teamForOperation(root string, scope teams.Scope, teamID string, actor teams.Actor) (teams.Team, sessionlog.TeamProjection, error) {
	if teams.ValidateID(teamID) != nil {
		return teams.Team{}, sessionlog.TeamProjection{}, teams.ErrNotFound
	}
	projection, err := sessionlog.ReplayTeams(root, scope.SessionID, teamID)
	if err != nil {
		return teams.Team{}, sessionlog.TeamProjection{}, err
	}
	team := projection.Teams[teamID]
	if team.Status != teams.TeamOpen || !team.Scope.Matches(scope) {
		return teams.Team{}, sessionlog.TeamProjection{}, teams.ErrPermission
	}
	if !actor.Lead {
		member, ok := projection.Members[actor.MemberID]
		if !ok || member.TeamID != teamID || member.Status.IsTerminal() {
			return teams.Team{}, sessionlog.TeamProjection{}, teams.ErrPermission
		}
	}
	return team, projection, nil
}

func teamTaskGraph(projection sessionlog.TeamProjection, teamID string) (*teams.TaskGraph, error) {
	tasks := make([]teams.Task, 0, len(projection.Tasks))
	for _, task := range projection.Tasks {
		if task.TeamID == teamID {
			tasks = append(tasks, task)
		}
	}
	graph, err := teams.LoadTaskGraph(teamID, tasks)
	if err != nil {
		return nil, err
	}
	return graph, nil
}
