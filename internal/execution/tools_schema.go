package execution

import (
	"sort"

	"stable/internal/todo"
)

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
