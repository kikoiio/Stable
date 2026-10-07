package agent

import (
	"context"
	"errors"
	"sync"

	"stable/internal/llm"
)

// TeamToolExecutor is supplied by a host bound to one trusted member/turn. The
// request is constructed by the child runner, never by the tool's arguments.
type TeamToolExecutor func(context.Context, ExecutionRequest, llm.ToolUse) (ToolOutcome, error)

// TeamToolHost lets the runtime construct executors before the conversation
// service exists, then bind the service once Serve has initialized it.
type TeamToolHost struct {
	mu       sync.RWMutex
	executor TeamToolExecutor
}

func (h *TeamToolHost) Bind(executor TeamToolExecutor) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.executor = executor
	h.mu.Unlock()
}

func (h *TeamToolHost) Execute(ctx context.Context, request ExecutionRequest, call llm.ToolUse) (ToolOutcome, error) {
	if h == nil {
		return TeamToolExecutorUnavailable(ctx, request, call)
	}
	h.mu.RLock()
	executor := h.executor
	h.mu.RUnlock()
	if executor == nil {
		return TeamToolExecutorUnavailable(ctx, request, call)
	}
	return executor(ctx, request, call)
}

func TeamToolExecutorUnavailable(_ context.Context, _ ExecutionRequest, call llm.ToolUse) (ToolOutcome, error) {
	return ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: ToolFailed, IsError: true, Content: "Error: team service is unavailable"}, nil
}

type teamMemberExecutorFactory struct {
	inspection        ExecutorFactory
	allowedInspection map[string]bool
	host              TeamToolExecutor
}

func NewTeamMemberExecutorFactory(inspection ExecutorFactory, allowedInspection []string, host TeamToolExecutor) (ExecutorFactory, error) {
	if inspection == nil || host == nil {
		return nil, errors.New("team member requires an inspection executor and scoped host tools")
	}
	allowed, err := inspectionToolSet(allowedInspection)
	if err != nil {
		return nil, err
	}
	return teamMemberExecutorFactory{inspection: inspection, allowedInspection: allowed, host: host}, nil
}

func (f teamMemberExecutorFactory) ForRun(request ExecutionRequest) (RunExecutor, error) {
	inner, err := f.inspection.ForRun(request)
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, errors.New("team inspection factory returned no executor")
	}
	return teamMemberExecutor{inner: inner, allowedInspection: f.allowedInspection, host: f.host, request: request}, nil
}

type teamMemberExecutor struct {
	inner             RunExecutor
	allowedInspection map[string]bool
	host              TeamToolExecutor
	request           ExecutionRequest
}

func (e teamMemberExecutor) Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error) {
	if e.allowedInspection[call.Name] {
		return e.inner.Execute(ctx, call)
	}
	if isTeamTool(call.Name) && teamMemberTools()[call.Name] {
		return e.host(ctx, e.request, call)
	}
	return ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: ToolDenied, IsError: true, Content: "Error: tool is not allowed for the read-only team member"}, nil
}

func TeamMemberToolSchemas(schemas []llm.ToolSchema, allowedInspection []string) ([]llm.ToolSchema, error) {
	inspection, err := inspectionToolSet(allowedInspection)
	if err != nil {
		return nil, err
	}
	allowed := teamMemberTools()
	for name := range inspection {
		allowed[name] = true
	}
	seen := map[string]bool{}
	out := make([]llm.ToolSchema, 0, len(schemas))
	for _, schema := range schemas {
		if allowed[schema.Name] && !seen[schema.Name] {
			out = append(out, schema)
			seen[schema.Name] = true
		}
	}
	return out, nil
}
