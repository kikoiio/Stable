package execution

import (
	"sort"

	"stable/internal/llm"
	"stable/internal/todo"
	"stable/internal/tools"
)

// DelegationTasksSchema exposes one synchronous batch call to a session parent.
var DelegationTasksSchema = map[string]any{
	"name":        "delegate_tasks",
	"description": "Delegate several independent read-only investigations in parallel and wait for every result.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":        "array",
				"description": "Independent, clearly scoped read-only tasks.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":          map[string]any{"type": "string"},
						"name":        map[string]any{"type": "string"},
						"instruction": map[string]any{"type": "string"},
					},
					"required":             []string{"id", "name", "instruction"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"tasks"},
		"additionalProperties": false,
	},
}

func DelegationToolSchemas() []llm.ToolSchema {
	return []llm.ToolSchema{schemaToLLM(DelegationTasksSchema)}
}

// ReadOnlyToolSchemas returns only the existing project inspection tools. The
// child executor enforces the same allowlist independently of these schemas.
func ReadOnlyToolSchemas() []llm.ToolSchema {
	registry := tools.CreateDefaultTools().Registry
	wanted := map[string]bool{"read_file": true, "glob": true, "grep": true}
	var schemas []llm.ToolSchema
	for _, item := range registry.GetAllSchemas() {
		name, _ := item["name"].(string)
		if !wanted[name] {
			continue
		}
		description, _ := item["description"].(string)
		input, _ := item["input_schema"].(map[string]any)
		schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

// WorkspaceWriterToolSchemas returns the fixed session workspace surface.
// The executor independently enforces this same allowlist.
func WorkspaceWriterToolSchemas() []llm.ToolSchema {
	registry := tools.CreateDefaultTools().Registry
	wanted := map[string]bool{"read_file": true, "glob": true, "grep": true, "write_file": true, "edit_file": true, "command": true}
	var schemas []llm.ToolSchema
	for _, item := range registry.GetAllSchemas() {
		name, _ := item["name"].(string)
		if !wanted[name] {
			continue
		}
		description, _ := item["description"].(string)
		input, _ := item["input_schema"].(map[string]any)
		schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	if len(schemas) != len(wanted) {
		return nil
	}
	return schemas
}

func schemaToLLM(item map[string]any) llm.ToolSchema {
	name, _ := item["name"].(string)
	description, _ := item["description"].(string)
	input, _ := item["input_schema"].(map[string]any)
	return llm.ToolSchema{Name: name, Description: description, InputSchema: input}
}

// AskUserSchema is the provider-facing schema of the ask_user tool. Field
// names follow the source AskUserQuestion tool; the executor validates the
// 1-4 questions and 2-4 options before handing them to the QuestionSink.
var AskUserSchema = map[string]any{
	"name": "ask_user",
	"description": `Ask the user a question with structured multiple-choice options. Use this to:
- Gather user preferences or requirements
- Clarify ambiguous instructions
- Get decisions on implementation choices
- Offer choices about direction to take

Each question has 2-4 options. An "Other" option for custom input is automatically provided.
Use multiSelect: true when choices are not mutually exclusive.`,
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"questions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type":        "string",
							"description": "The complete question to ask the user. Should end with a question mark.",
						},
						"header": map[string]any{
							"type":        "string",
							"description": "Very short label displayed as a chip/tag (max 12 chars)",
							"maxLength":   12,
						},
						"options": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label": map[string]any{
										"type":        "string",
										"description": "The display text for this option (1-5 words)",
									},
									"description": map[string]any{
										"type":        "string",
										"description": "Explanation of what this option means or what will happen if chosen",
									},
								},
								"required": []string{"label", "description"},
							},
							"minItems": 2,
							"maxItems": 4,
						},
						"multiSelect": map[string]any{
							"type":    "boolean",
							"default": false,
						},
					},
					"required": []string{"question", "header", "options", "multiSelect"},
				},
				"minItems": 1,
				"maxItems": 4,
			},
		},
		"required": []string{"questions"},
	},
}

// ExitPlanModeSchema is the provider-facing schema of the exit_plan_mode tool.
// The tool takes no arguments; calling it presents the completed plan for user
// approval and ends the turn.
var ExitPlanModeSchema = map[string]any{
	"name":        "exit_plan_mode",
	"description": "Exit plan mode and present the plan for user approval. Call this when your plan is complete and written to the plan file.",
	"input_schema": map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	},
}

// m06TaskSchema wraps one of the internal/todo provider schemas with its
// controlled model name and a task-facility description prefix.
func m06TaskSchema(base map[string]any) map[string]any {
	name, _ := base["name"].(string)
	description, _ := base["description"].(string)
	schema := map[string]any{
		"name":        name,
		"description": "Manage the current session task list. " + description,
	}
	if input, ok := base["input_schema"].(map[string]any); ok {
		schema["input_schema"] = input
	}
	return schema
}

// The task_* schemas reuse the internal/todo schema constants, wrapped with
// their controlled tool names for the executor dispatch and whitelists.
var (
	TaskCreateSchema = m06TaskSchema(todo.TaskCreateSchema)
	TaskGetSchema    = m06TaskSchema(todo.TaskGetSchema)
	TaskListSchema   = m06TaskSchema(todo.TaskListSchema)
	TaskUpdateSchema = m06TaskSchema(todo.TaskUpdateSchema)
)

// LoadSkillSchema is the provider-facing schema of the load_skill tool. The
// executor resolves the name through the host SkillProvider; skill names come
// from the session's skill inventory, not from this static description.
var LoadSkillSchema = map[string]any{
	"name": "load_skill",
	"description": `Load the full instructions of a skill by name and activate it for the current session. Use this when the skill inventory or a delta notice lists a skill relevant to the current task.

The skill body is returned as the tool result and its guidance applies to the conversation from this point on. Call it again later to re-read the freshest body (for example after context compaction).`,
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The skill name, exactly as listed in the available skills inventory.",
			},
			"args": map[string]any{
				"type":        "string",
				"description": "Optional arguments forwarded into the skill body. When the skill body contains an $ARGUMENTS placeholder it is substituted; otherwise the arguments are appended as the user request section.",
			},
		},
		"required": []string{"name"},
	},
}

// MCPCallSchema is the provider-facing schema of the mcp_call bridge tool.
// It reaches dispatch-tier tools that are not exposed in the tool inventory:
// the server/tool pair is resolved against the full MCP inventory (the pair
// itself, the full "mcp__server__tool" name, or a unique bare tool name) and
// the arguments are coerced against the target schema before the gated call.
var MCPCallSchema = map[string]any{
	"name": "mcp_call",
	"description": `Call a tool on a connected MCP server by name. Use this for dispatch-tier tools that are not listed in your tool inventory; run tool_search first to discover them.

- server: the MCP server providing the tool, and tool: the tool name on that server. The pair may also be given as the full "mcp__server__tool" name, or as a bare tool name when it is unique across servers.
- arguments: the tool input object shaped after the target tool's input schema; it is corrected against the schema before the call.
The call passes the same permission gate as direct MCP tool calls and may require user approval.`,
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "The MCP server that provides the tool.",
			},
			"tool": map[string]any{
				"type":        "string",
				"description": "The tool name on that server, exactly as reported by tool_search.",
			},
			"arguments": map[string]any{
				"type":        "object",
				"description": "The tool input object matching the target tool's input schema. Omit for tools without parameters.",
			},
		},
		"required": []string{"server", "tool"},
	},
}

// ToolSearchSchema is the provider-facing schema of the tool_search tool. It
// is read-only: it lists the dispatch tier without calling anything and
// constructs no permission operation.
var ToolSearchSchema = map[string]any{
	"name":        "tool_search",
	"description": `Search the dispatch tier of the connected MCP servers for callable tools. Dispatch-tier tools do not appear in your tool inventory, so run this search when no listed tool fits the task, then invoke what you find with mcp_call or its full mcp__server__tool name. The search is read-only: it never executes a tool and needs no approval.`,
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Space-separated keywords, each matched as a substring against tool names and descriptions. An empty query lists every dispatch tool.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     20,
				"description": "Maximum number of tools to return. Defaults to 20, which is also the hard cap.",
			},
		},
	},
}

// M06ToolSchemas returns the six M06 tool schemas in model-name order, ready
// for the chatserve/runtime schema whitelists and executor dispatch tests.
func M06ToolSchemas() []map[string]any {
	schemas := []map[string]any{
		AskUserSchema,
		ExitPlanModeSchema,
		TaskCreateSchema,
		TaskGetSchema,
		TaskListSchema,
		TaskUpdateSchema,
	}
	sort.Slice(schemas, func(i, j int) bool {
		left, _ := schemas[i]["name"].(string)
		right, _ := schemas[j]["name"].(string)
		return left < right
	})
	return schemas
}

// SkillToolSchemas returns the M07-A tool schemas in model-name order, ready
// for the chatserve/runtime schema whitelists and executor dispatch tests.
func SkillToolSchemas() []map[string]any {
	return []map[string]any{LoadSkillSchema}
}

// MCPToolSchemas returns the two dispatch entry points in provider schema
// form. Eager MCP schemas are appended by the host after connection setup.
func MCPToolSchemas() []map[string]any {
	return []map[string]any{MCPCallSchema, ToolSearchSchema}
}
