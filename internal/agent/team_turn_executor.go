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
	roleTools map[string]bool
	role      ExecutorFactory
	host      TeamToolExecutor
	worktree  bool
}

func NewTeamMemberExecutorFactory(inspection ExecutorFactory, allowedInspection []string, host TeamToolExecutor) (ExecutorFactory, error) {
	if inspection == nil || host == nil {
		return nil, errors.New("team member requires an inspection executor and scoped host tools")
	}
	allowed, err := inspectionToolSet(allowedInspection)
	if err != nil {
		return nil, err
	}
	return teamMemberExecutorFactory{roleTools: allowed, role: inspection, host: host}, nil
}

// NewWorktreeTeamMemberExecutorFactory binds a worktree-isolated team member
// to the trusted writer factory for that member's leased workspace. The role
// tool list is trusted catalog data; provider supplied tool names never expand
// it. The writer factory remains responsible for workspace and permission
// enforcement.
func NewWorktreeTeamMemberExecutorFactory(writer ExecutorFactory, allowedRoleTools []string, host TeamToolExecutor) (ExecutorFactory, error) {
	if writer == nil || host == nil {
		return nil, errors.New("worktree team member requires a workspace writer executor and scoped host tools")
	}
	allowed, err := worktreeTeamRoleToolSet(allowedRoleTools)
	if err != nil {
		return nil, err
	}
	return teamMemberExecutorFactory{roleTools: allowed, role: writer, host: host, worktree: true}, nil
}

func (f teamMemberExecutorFactory) ForRun(request ExecutionRequest) (RunExecutor, error) {
	inner, err := f.role.ForRun(request)
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, errors.New("team inspection factory returned no executor")
	}
	return teamMemberExecutor{inner: inner, allowedRoleTools: f.roleTools, host: f.host, request: request, worktree: f.worktree}, nil
}

type teamMemberExecutor struct {
	inner            RunExecutor
	allowedRoleTools map[string]bool
	host             TeamToolExecutor
	request          ExecutionRequest
	worktree         bool
}

func (e teamMemberExecutor) Execute(ctx context.Context, call llm.ToolUse) (ToolOutcome, error) {
	if e.allowedRoleTools[call.Name] {
		return e.inner.Execute(ctx, call)
	}
	if isTeamTool(call.Name) && teamMemberTools()[call.Name] {
		return e.host(ctx, e.request, call)
	}
	message := "Error: tool is not allowed for the read-only team member"
	if e.worktree {
		message = "Error: tool is not allowed for this worktree team member"
	}
	return ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: ToolDenied, IsError: true, Content: message}, nil
}

func TeamMemberToolSchemas(schemas []llm.ToolSchema, allowedInspection []string) ([]llm.ToolSchema, error) {
	inspection, err := inspectionToolSet(allowedInspection)
	if err != nil {
		return nil, err
	}
	return filterTeamMemberSchemas(schemas, inspection), nil
}

// WorktreeTeamMemberToolSchemas exposes only tools declared by the trusted
// worktree role plus scoped team tools. The caller supplies the application's
// workspace-writer schema inventory, never a provider-selected inventory.
func WorktreeTeamMemberToolSchemas(schemas []llm.ToolSchema, allowedRoleTools []string) ([]llm.ToolSchema, error) {
	roleTools, err := worktreeTeamRoleToolSet(allowedRoleTools)
	if err != nil {
		return nil, err
	}
	return filterTeamMemberSchemas(schemas, roleTools), nil
}

func filterTeamMemberSchemas(schemas []llm.ToolSchema, roleTools map[string]bool) []llm.ToolSchema {
	allowed := teamMemberTools()
	for name := range roleTools {
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
	return out
}

func worktreeTeamRoleToolSet(names []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		switch name {
		case "read_file", "glob", "grep", "write_file", "edit_file", "command":
			allowed[name] = true
		default:
			return nil, errors.New("worktree team role requested a tool outside the workspace writer allowlist")
		}
	}
	return allowed, nil
}
