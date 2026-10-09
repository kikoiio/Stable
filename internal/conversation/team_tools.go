package conversation

import (
	"context"
	"encoding/json"
	"errors"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/teams"
)

func (s *Service) ExecuteTeamTool(ctx context.Context, request agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
	out := agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolFailed, IsError: true}
	root, scope, actor, authErr := s.teamOperationScope(ctx, request)
	if authErr != nil {
		out.Status, out.Content = agent.ToolDenied, "Error: team operation is not authorized for this run"
		return out, nil
	}
	var args struct {
		Name               *string   `json:"name"`
		MemberName         string    `json:"member_name"`
		AgentName          string    `json:"agent_name"`
		Instruction        string    `json:"instruction"`
		PlanRequired       bool      `json:"plan_required"`
		TeamID             string    `json:"team_id"`
		MemberID           string    `json:"member_id"`
		TaskID             string    `json:"task_id"`
		Title              *string   `json:"title"`
		Description        *string   `json:"description"`
		Assignee           *string   `json:"assignee"`
		BlockedBy          *[]string `json:"blocked_by"`
		Status             *string   `json:"status"`
		ExpectedRevision   uint64    `json:"expected_revision"`
		Recipient          string    `json:"recipient"`
		Body               string    `json:"body"`
		Broadcast          bool      `json:"broadcast"`
		AfterSeq           uint64    `json:"after_seq"`
		AfterTaskID        string    `json:"after_task_id"`
		AfterTeamID        string    `json:"after_team_id"`
		Limit              int       `json:"limit"`
		RequestID          string    `json:"request_id"`
		AfterTeamRequestID string    `json:"after_team_request_id"`
		AfterMemberID      string    `json:"after_member_id"`
		Decision           string    `json:"decision"`
		Feedback           string    `json:"feedback"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		out.Content = "Error: invalid team tool arguments"
		return out, nil
	}
	memberTool := map[string]bool{"team_get": true, "team_member_get": true, "team_member_list": true, "team_send": true, "team_messages": true, "team_plan_submit": true, "team_request_list": true, "team_request_respond": true, "team_task_create": true, "team_task_get": true, "team_task_list": true, "team_task_update": true}
	if !actor.Lead && !memberTool[call.Name] {
		out.Status, out.Content = agent.ToolDenied, "Error: this team operation is lead-only"
		return out, nil
	}
	if !actor.Lead && args.TeamID != request.TeamTurn.TeamID {
		out.Status, out.Content = agent.ToolDenied, "Error: team member is scoped to a different team"
		return out, nil
	}
	var result any
	var err error
	switch call.Name {
	case "team_create":
		if args.Name == nil {
			err = errors.New("team name is required")
		} else {
			result, err = s.CreateTeam(ctx, request, *args.Name)
		}
	case "team_member_spawn":
		if args.TeamID == "" || call.ID == "" {
			err = errors.New("team ID and tool call ID are required")
		} else {
			result, err = s.SpawnTeamMember(ctx, request, TeamMemberSpawnRequest{TeamID: args.TeamID, Name: args.MemberName, AgentName: args.AgentName, Instruction: args.Instruction, PlanRequired: args.PlanRequired, OriginCallID: call.ID})
		}
	case "team_member_resume":
		if args.TeamID == "" || args.MemberID == "" || call.ID == "" {
			err = errors.New("team ID, member ID and tool call ID are required")
		} else {
			result, err = s.ResumeTeamMember(ctx, request, args.TeamID, args.MemberID, call.ID)
		}
	case "team_list":
		result, err = s.ListTeamsPage(ctx, request, args.AfterTeamID, args.Limit)
	case "team_get":
		if args.TeamID == "" {
			err = teams.ErrNotFound
		} else {
			result, err = s.GetTeam(ctx, request, args.TeamID)
		}
	case "team_member_get", "team_member_list":
		team, projection, projectionErr := s.teamForOperation(root, scope, args.TeamID, actor)
		if projectionErr != nil {
			err = projectionErr
		} else if call.Name == "team_member_get" {
			member, ok := projection.Members[args.MemberID]
			if !ok || member.TeamID != team.ID {
				err = teams.ErrNotFound
			} else {
				result = member
			}
		} else {
			members := make([]teams.Member, 0)
			for _, member := range projection.Members {
				if member.TeamID == team.ID {
					members = append(members, member)
				}
			}
			result, err = teams.MembersPageAfter(members, args.AfterMemberID, args.Limit)
		}
	case "team_close":
		if args.TeamID == "" {
			err = teams.ErrNotFound
		} else {
			result, err = s.CloseTeam(ctx, request, args.TeamID)
		}
	case "team_send":
		if args.TeamID == "" || call.ID == "" {
			err = errors.New("team ID and tool call ID are required")
		} else {
			result, err = s.SendTeamMessage(ctx, request, TeamSendRequest{TeamID: args.TeamID, Recipient: args.Recipient, Body: args.Body, Token: request.RunID + ":" + call.ID, Broadcast: args.Broadcast})
		}
	case "team_messages":
		result, err = s.ListTeamMessages(ctx, request, args.TeamID, args.AfterSeq, args.Limit)
	case "team_plan_submit":
		result, err = s.SubmitTeamPlan(ctx, request, args.TeamID, args.Body)
	case "team_request_list":
		result, err = s.ListTeamRequestsPage(ctx, request, args.TeamID, args.AfterTeamRequestID, args.Limit)
	case "team_request_respond":
		result, err = s.RespondTeamRequest(ctx, request, args.TeamID, args.RequestID, args.ExpectedRevision, args.Decision, args.Feedback)
	case "team_shutdown_request":
		result, err = s.RequestTeamShutdown(ctx, request, args.TeamID, args.MemberID)
	case "team_task_create":
		if args.TeamID == "" || args.Title == nil {
			err = errors.New("team ID and task title are required")
		} else {
			task := teams.Task{Title: *args.Title}
			if args.Description != nil {
				task.Description = *args.Description
			}
			if args.Assignee != nil {
				task.Assignee = *args.Assignee
			}
			if args.BlockedBy != nil {
				task.BlockedBy = *args.BlockedBy
			}
			result, err = s.CreateTeamTask(ctx, request, args.TeamID, task)
		}
	case "team_task_get":
		result, err = s.GetTeamTask(ctx, request, args.TeamID, args.TaskID)
	case "team_task_list":
		result, err = s.ListTeamTasksPage(ctx, request, args.TeamID, args.AfterTaskID, args.Limit)
	case "team_task_update":
		if args.Status != nil {
			status := teams.TaskStatus(*args.Status)
			patch := teams.TaskPatch{Title: args.Title, Description: args.Description, Assignee: args.Assignee, BlockedBy: args.BlockedBy, Status: &status}
			result, err = s.UpdateTeamTask(ctx, request, args.TeamID, args.TaskID, args.ExpectedRevision, patch)
		} else {
			patch := teams.TaskPatch{Title: args.Title, Description: args.Description, Assignee: args.Assignee, BlockedBy: args.BlockedBy}
			result, err = s.UpdateTeamTask(ctx, request, args.TeamID, args.TaskID, args.ExpectedRevision, patch)
		}
	default:
		out.Content = "Error: unsupported team operation"
		return out, nil
	}
	if err != nil {
		out.Content = "Error: " + err.Error()
		return out, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return out, err
	}
	out.Status, out.IsError, out.Content = agent.ToolSucceeded, false, string(encoded)
	return out, nil
}
