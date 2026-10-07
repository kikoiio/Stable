package conversation

import (
	"context"
	"errors"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func (s *Service) handleAgentRequest(ctx context.Context, msg ClientMsg) ([]ServerMsg, error) {
	// Resolve the session before any catalog reload or execution.
	if _, err := sessionlog.Replay(s.deps.ProjectRoot, msg.SessionID); err != nil {
		return nil, err
	}
	switch msg.Op {
	case "agent_list", "agent_reload":
		if s.deps.Agents == nil {
			return nil, errors.New("agent catalog is unavailable")
		}
		snapshot := s.deps.Agents.Snapshot()
		if msg.Op == "agent_reload" {
			snapshot = s.deps.Agents.Reload()
		}
		return []ServerMsg{{Type: msg.Op, Agents: &snapshot}}, nil
	case "agent_task_list":
		tasks, err := s.listAgentTasks(msg.SessionID, msg.AfterSeq, msg.Limit)
		return []ServerMsg{{Type: "agent_task_list", AgentTasks: tasks}}, err
	}
	if s.deps.AgentTasks == nil {
		return nil, errors.New("agent tasks are unavailable")
	}
	parent := agent.ParentRun{Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: msg.SessionID}}
	var snapshot agent.AgentTaskSnapshot
	var err error
	switch msg.Op {
	case "agent_task_start":
		id, idErr := sessionlog.NewID()
		if idErr != nil {
			return nil, idErr
		}
		parent, err = s.forkParentRun(ctx, msg.SessionID, "agent-entry-"+id)
		if err == nil {
			snapshot, err = s.deps.AgentTasks.run(ctx, parent, agent.AgentTaskRequest{AgentName: msg.AgentName, Instruction: msg.Text, Background: true, Model: msg.Model, Timeout: time.Duration(msg.TimeoutMS) * time.Millisecond, Isolation: msg.Isolation}, true)
		}
	case "agent_task_get":
		snapshot, err = s.deps.AgentTasks.Output(ctx, parent, msg.TaskID, time.Duration(msg.WaitMS)*time.Millisecond)
	case "agent_task_cancel":
		snapshot, err = s.deps.AgentTasks.Stop(ctx, parent, msg.TaskID)
	}
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: msg.Op, AgentTask: &snapshot}}, nil
}
