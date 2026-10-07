package conversation

import (
	"context"
	"encoding/json"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

func (s *Service) hookAgentParent(request agent.ExecutionRequest) agent.ParentRun {
	projectRoot := s.deps.ProjectRoot
	var authority permission.Authority
	if json.Unmarshal(request.PermissionBounds, &authority) == nil && authority.AllowedRoot != "" {
		projectRoot = authority.AllowedRoot
	}
	return agent.ParentRun{
		RunID: request.RunID, Deadline: request.RunDeadline, Work: request.Work,
		ProjectRoot: projectRoot, PermissionBounds: append(json.RawMessage(nil), request.PermissionBounds...),
		Provider: s.deps.ForkProvider, ProviderName: request.ProviderName, Model: request.Model,
		ToolSchemas: append([]llm.ToolSchema(nil), s.deps.ForkToolSchemas...), ExecutorFactory: s.deps.ForkExecutorFactory,
	}
}

func (s *Service) hookAgentServiceContext() context.Context {
	if s.hooks != nil {
		return s.hooks.serviceContext()
	}
	if s.lifeCtx != nil {
		return s.lifeCtx
	}
	return context.Background()
}
