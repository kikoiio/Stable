package execution

import (
	"context"
	"sync"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/workspace"
)

// WorkspaceLifecycleToolHost is a service-bound capability. The run executor
// exposes these tools only to its trusted lead factory; child factories drop
// the host even if they inherit the same base executor configuration.
type WorkspaceLifecycleToolHost struct {
	mu      sync.RWMutex
	execute func(context.Context, agent.ExecutionRequest, *workspace.WriterLease, llm.ToolUse) (agent.ToolOutcome, error)
}

func NewWorkspaceLifecycleToolHost() *WorkspaceLifecycleToolHost {
	return &WorkspaceLifecycleToolHost{}
}

func (h *WorkspaceLifecycleToolHost) Bind(execute func(context.Context, agent.ExecutionRequest, *workspace.WriterLease, llm.ToolUse) (agent.ToolOutcome, error)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.execute = execute
	h.mu.Unlock()
}

func (h *WorkspaceLifecycleToolHost) Execute(ctx context.Context, request agent.ExecutionRequest, lease *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
	if h == nil {
		return agent.ToolOutcome{}, workspace.ErrUnavailable
	}
	h.mu.RLock()
	execute := h.execute
	h.mu.RUnlock()
	if execute == nil {
		return agent.ToolOutcome{}, workspace.ErrUnavailable
	}
	return execute(ctx, request, lease, call)
}

func WorkspaceLifecycleToolSchemas() []llm.ToolSchema {
	return []llm.ToolSchema{
		{Name: "enter_worktree", Description: "Schedule entering an owned workspace for the next run. If workspace_id is omitted, create a workspace using the optional label and enter it after this run ends.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"workspace_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Service-issued workspace ID owned by this session/work scope."},
				"label":        map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Optional label when creating a new workspace."},
			}, "additionalProperties": false,
		}},
		{Name: "exit_worktree", Description: "Schedule leaving the current workspace after this run ends; the workspace is kept.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{}, "additionalProperties": false,
		}},
		{Name: "worktree_export", Description: "Export an owned, non-writing workspace as a reviewable candidate. Export is deferred until this lead run ends when it owns the workspace writer lease.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"workspace_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Service-issued workspace ID owned by this session/work scope."},
			}, "required": []string{"workspace_id"}, "additionalProperties": false,
		}},
	}
}

func isWorkspaceLifecycleTool(name string) bool {
	switch name {
	case "enter_worktree", "exit_worktree", "worktree_export":
		return true
	default:
		return false
	}
}
