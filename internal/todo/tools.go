package todo

// Provider-facing tool schemas of the todo facility. Names match the
// controlled executor dispatch and the chatserve/runtime schema whitelists;
// field names follow the source task tools, with the deleted status added to
// task_update.
var (
	// TaskCreateSchema creates one task; subject and description are required.
	TaskCreateSchema = map[string]any{
		"name":        "task_create",
		"description": "Create a new task to track work. Use this to break complex work into smaller, trackable steps before starting implementation.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subject":     map[string]any{"type": "string", "description": "A brief title for the task"},
				"description": map[string]any{"type": "string", "description": "What needs to be done"},
				"activeForm": map[string]any{
					"type":        "string",
					"description": `Present continuous form shown in spinner when in_progress (e.g. "Running tests")`,
				},
				"metadata": map[string]any{
					"type":                 "object",
					"description":          "Arbitrary metadata to attach to the task",
					"additionalProperties": map[string]any{"type": "string"},
				},
			},
			"required": []string{"subject", "description"},
		},
	}

	// TaskGetSchema returns the full detail of one task by id.
	TaskGetSchema = map[string]any{
		"name":        "task_get",
		"description": "Get the details of a specific task by its ID.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"taskId": map[string]any{"type": "string", "description": "The ID of the task to retrieve"},
			},
			"required": []string{"taskId"},
		},
	}

	// TaskListSchema lists every task of the current session.
	TaskListSchema = map[string]any{
		"name":        "task_list",
		"description": "List all tasks in the current session task list. Shows ID, status, subject, and blocking info.",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}

	// TaskUpdateSchema mutates one task; the deleted status removes it.
	TaskUpdateSchema = map[string]any{
		"name":        "task_update",
		"description": `Update a task's status, subject, description, or dependencies. Set status to "in_progress" when starting work and "completed" when done; status transitions run pending -> in_progress -> completed and may also step back. Set status to "deleted" to remove the task.`,
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"taskId":      map[string]any{"type": "string", "description": "The ID of the task to update"},
				"subject":     map[string]any{"type": "string", "description": "New subject for the task"},
				"description": map[string]any{"type": "string", "description": "New description for the task"},
				"activeForm":  map[string]any{"type": "string", "description": "Present continuous form shown in spinner when in_progress"},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"pending", "in_progress", "completed", "deleted"},
					"description": `New status for the task; "deleted" removes it`,
				},
				"addBlocks": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Task IDs that this task blocks",
				},
				"addBlockedBy": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Task IDs that block this task",
				},
				"owner": map[string]any{"type": "string", "description": "New owner for the task"},
				"metadata": map[string]any{
					"type":                 "object",
					"description":          "Arbitrary metadata to attach to the task",
					"additionalProperties": map[string]any{"type": "string"},
				},
			},
			"required": []string{"taskId"},
		},
	}
)
