package execution

import "stable/internal/llm"

var teamCoordinatorTools = map[string]bool{
	"team_member_spawn": true, "team_member_resume": true,
	"team_list": true, "team_get": true, "team_member_get": true, "team_member_list": true,
	"team_send": true, "team_messages": true,
	"team_request_list": true, "team_request_respond": true, "team_shutdown_request": true,
	"team_task_create": true, "team_task_get": true, "team_task_list": true, "team_task_update": true,
}

// TeamCoordinatorToolAllowed is the hard executor allowlist. Provider schemas
// are filtered separately, so forged direct calls are denied too.
func TeamCoordinatorToolAllowed(name string) bool { return teamCoordinatorTools[name] }

func TeamCoordinatorToolSchemas(schemas []llm.ToolSchema) []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(teamCoordinatorTools))
	seen := map[string]bool{}
	for _, schema := range schemas {
		if teamCoordinatorTools[schema.Name] && !seen[schema.Name] {
			out = append(out, schema)
			seen[schema.Name] = true
		}
	}
	return out
}

// TeamToolSchemas exposes only operations backed by the durable conversation
// team service and shared-pool member lifecycle.
func TeamToolSchemas() []map[string]any {
	stringField := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	uintField := map[string]any{"type": "integer", "minimum": 1, "description": "Revision observed by the caller"}
	arrayString := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	obj := func(name, description string, properties map[string]any, required ...string) map[string]any {
		return map[string]any{"name": name, "description": description, "input_schema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
	}
	return []map[string]any{
		obj("team_create", "Create a team scoped to this run's session or goal.", map[string]any{"name": stringField("Unique active team name")}, "name"),
		obj("team_member_spawn", "Start a bounded read-only team member in the shared child pool.", map[string]any{"team_id": stringField("Service-issued team ID"), "member_name": stringField("Unique active member name"), "agent_name": stringField("Read-only role name"), "instruction": stringField("Assigned task for this member"), "plan_required": map[string]any{"type": "boolean", "description": "Require lead plan approval before follow-up work"}}, "team_id", "member_name", "agent_name", "instruction"),
		obj("team_member_resume", "Resume an idle or interrupted member with its bounded pending messages and unfinished tasks.", map[string]any{"team_id": stringField("Service-issued team ID"), "member_id": stringField("Service-issued member ID")}, "team_id", "member_id"),
		obj("team_list", "List a bounded page of teams visible in this run's exact work scope (default 20, maximum 100).", map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}),
		obj("team_get", "Get a team in this run's exact work scope.", map[string]any{"team_id": stringField("Service-issued team ID")}, "team_id"),
		obj("team_member_get", "Get a member of a team visible to this trusted run.", map[string]any{"team_id": stringField("Service-issued team ID"), "member_id": stringField("Service-issued member ID")}, "team_id", "member_id"),
		obj("team_member_list", "List a bounded page of team members visible to this trusted run (default 20, maximum 100).", map[string]any{"team_id": stringField("Service-issued team ID"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "team_id"),
		obj("team_close", "Begin closing a team in this run's exact work scope.", map[string]any{"team_id": stringField("Service-issued team ID")}, "team_id"),
		obj("team_send", "Send a persistent message to a team member, the lead, or all active members.", map[string]any{"team_id": stringField("Service-issued team ID"), "recipient": stringField("Member ID or lead; omit for broadcast"), "body": stringField("Message body"), "broadcast": map[string]any{"type": "boolean", "description": "Send to the fixed snapshot of active members"}}, "team_id", "body"),
		obj("team_messages", "Read a bounded page of messages visible to this team actor.", map[string]any{"team_id": stringField("Service-issued team ID"), "after_seq": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "team_id"),
		obj("team_plan_submit", "Submit the initial read-only plan for lead approval.", map[string]any{"team_id": stringField("Service-issued team ID"), "body": stringField("Plan text, up to 8 KiB")}, "team_id", "body"),
		obj("team_request_list", "List a bounded page of pending and historical team requests visible to this actor (default 20, maximum 100).", map[string]any{"team_id": stringField("Service-issued team ID"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "team_id"),
		obj("team_request_respond", "Respond to a plan or shutdown request using its current revision.", map[string]any{"team_id": stringField("Service-issued team ID"), "request_id": stringField("Service-issued request ID"), "expected_revision": uintField, "decision": map[string]any{"type": "string", "enum": []string{"approved", "rejected", "deferred"}}, "feedback": stringField("Optional feedback, up to 2 KiB")}, "team_id", "request_id", "expected_revision", "decision"),
		obj("team_shutdown_request", "Ask one team member to shut down through a typed request.", map[string]any{"team_id": stringField("Service-issued team ID"), "member_id": stringField("Service-issued member ID")}, "team_id", "member_id"),
		obj("team_task_create", "Create a task on a team task board.", map[string]any{"team_id": stringField("Service-issued team ID"), "title": stringField("Task title"), "description": stringField("Task description"), "assignee": stringField("Team member ID or empty"), "blocked_by": arrayString}, "team_id", "title"),
		obj("team_task_get", "Get a task from a team task board.", map[string]any{"team_id": stringField("Service-issued team ID"), "task_id": stringField("Service-issued task ID")}, "team_id", "task_id"),
		obj("team_task_list", "List a bounded page of team task board entries (default 20, maximum 100).", map[string]any{"team_id": stringField("Service-issued team ID"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "team_id"),
		obj("team_task_update", "Update a task using its current revision.", map[string]any{"team_id": stringField("Service-issued team ID"), "task_id": stringField("Service-issued task ID"), "expected_revision": uintField, "title": stringField("Replacement title"), "description": stringField("Replacement description"), "assignee": stringField("Member ID or empty"), "blocked_by": arrayString, "status": map[string]any{"type": "string", "enum": []string{"pending", "blocked", "in_progress", "completed"}}}, "team_id", "task_id", "expected_revision"),
	}
}
