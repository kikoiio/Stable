package execution

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
)

func TestMemberSpawnAndResumeToolsReachTrustedTeamHost(t *testing.T) {
	host := &agent.TeamToolHost{}
	called := make(chan string, 2)
	host.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		called <- call.Name
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded, Content: "ok"}, nil
	})
	executor := &toolRunExecutor{deps: ToolExecutorDeps{TeamTools: host, Now: time.Now}}
	for _, name := range []string{"team_member_spawn", "team_member_resume"} {
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "call-" + name, Name: name, Arguments: []byte(`{}`)})
		if err != nil || outcome.Status != agent.ToolSucceeded || outcome.Content != "ok" {
			t.Fatalf("%s dispatch = %+v, %v", name, outcome, err)
		}
		if got := <-called; got != name {
			t.Fatalf("team host received %q, want %q", got, name)
		}
	}
}

func TestTeamCoordinatorSchemasAndExecutorUseStaticTeamOnlyAllowlist(t *testing.T) {
	wantAllowed := []string{
		"team_member_spawn", "team_member_resume",
		"team_list", "team_get", "team_member_get", "team_member_list",
		"team_send", "team_messages",
		"team_request_list", "team_request_respond", "team_shutdown_request",
		"team_task_create", "team_task_get", "team_task_list", "team_task_update",
	}

	// Keep the expected policy independent from the production allowlist map so
	// additions/removals require an intentional test update. The configured
	// provider schema order is preserved, duplicates are removed, and unrelated
	// tools are filtered out.
	configuredNames := []string{
		"read_file", "team_member_spawn", "write_file", "team_member_resume",
		"team_list", "team_get", "team_member_get", "team_member_list",
		"team_send", "team_messages", "team_request_list", "team_request_respond",
		"team_shutdown_request", "team_task_create", "team_task_get", "team_task_list",
		"team_task_update", "command", "team_member_spawn", "team_task_update", "team_create", "team_close",
	}
	all := make([]llm.ToolSchema, 0, len(configuredNames))
	for _, name := range configuredNames {
		all = append(all, llm.ToolSchema{Name: name})
	}
	filtered := TeamCoordinatorToolSchemas(all)
	gotAllowed := make([]string, 0, len(filtered))
	for _, schema := range filtered {
		gotAllowed = append(gotAllowed, schema.Name)
	}
	if !reflect.DeepEqual(gotAllowed, wantAllowed) {
		t.Fatalf("coordinator schemas = %v, want complete ordered allowlist %v", gotAllowed, wantAllowed)
	}
	for _, name := range wantAllowed {
		if !TeamCoordinatorToolAllowed(name) {
			t.Errorf("expected coordinator tool %q is missing from hard executor allowlist", name)
		}
	}
	for _, name := range []string{"read_file", "write_file", "command", "team_create", "team_close", "run_agent", "mcp_call"} {
		if TeamCoordinatorToolAllowed(name) {
			t.Errorf("unrelated tool %q unexpectedly entered hard coordinator allowlist", name)
		}
	}
	for _, name := range []string{"read_file", "write_file", "command", "mcp_call", "run_agent", "delegate_tasks", "task_update", "team_create", "team_close"} {
		executor := &toolRunExecutor{deps: ToolExecutorDeps{Now: time.Now}, request: agent.ExecutionRequest{TeamCoordinator: true}}
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "blocked", Name: name, Arguments: []byte(`{}`)})
		if err != nil || outcome.Status != agent.ToolDenied || !strings.Contains(outcome.Content, "coordinator mode") {
			t.Fatalf("coordinator direct call %s was not denied: %+v, %v", name, outcome, err)
		}
	}
}
