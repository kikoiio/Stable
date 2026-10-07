package execution

import "stable/internal/llm"

// AgentTaskToolSchemas is offered only to ordinary Session/Goal parents.
// Child schemas remain the independent inspection-only inventory.
func AgentTaskToolSchemas() []llm.ToolSchema {
	return []llm.ToolSchema{
		{Name: "run_agent", Description: "Run a named read-only project agent, or explicitly request the role's approved worktree isolation for bounded file edits. Wait for its result or return a background task ID.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"agent_name":  map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
				"instruction": map[string]any{"type": "string", "minLength": 1, "maxLength": 65536},
				"background":  map[string]any{"type": "boolean", "default": false},
				"model":       map[string]any{"type": "string", "maxLength": 256, "description": "Optional model on the parent's provider; credentials and provider cannot change."},
				"timeout_ms":  map[string]any{"type": "integer", "minimum": 1, "maximum": 180000, "description": "Optional shorter task timeout."},
				"isolation":   map[string]any{"type": "string", "enum": []string{"none", "worktree"}, "description": "Optional isolation choice; worktree requires a role that enables it."},
			}, "required": []string{"agent_name", "instruction"}, "additionalProperties": false,
		}},
		{Name: "task_output", Description: "Read a named agent task's status and summary in this session. Optionally wait up to 30 seconds without occupying a child worker.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"task_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
				"block":   map[string]any{"type": "boolean", "default": false},
				"wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000, "description": "Requires block=true; defaults to 30000 ms when blocking."},
			}, "required": []string{"task_id"}, "additionalProperties": false,
		}},
		{Name: "task_stop", Description: "Request cancellation of one named agent task in this session. Returns its actual current status; repeated cancellation is safe.", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}}, "required": []string{"task_id"}, "additionalProperties": false,
		}},
	}
}
