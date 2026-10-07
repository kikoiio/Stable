package execution

import (
	"context"
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
	all := []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}, {Name: "command"}, {Name: "run_agent"}, {Name: "team_create"}, {Name: "team_close"}, {Name: "team_list"}, {Name: "team_send"}, {Name: "team_task_update"}}
	filtered := TeamCoordinatorToolSchemas(all)
	if len(filtered) != 3 || filtered[0].Name != "team_list" || filtered[1].Name != "team_send" || filtered[2].Name != "team_task_update" {
		t.Fatalf("coordinator schemas include unsupported tools: %+v", filtered)
	}
	for _, name := range []string{"read_file", "write_file", "command", "mcp_call", "run_agent", "delegate_tasks", "task_update", "team_create", "team_close"} {
		executor := &toolRunExecutor{deps: ToolExecutorDeps{Now: time.Now}, request: agent.ExecutionRequest{TeamCoordinator: true}}
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "blocked", Name: name, Arguments: []byte(`{}`)})
		if err != nil || outcome.Status != agent.ToolDenied || !strings.Contains(outcome.Content, "coordinator mode") {
			t.Fatalf("coordinator direct call %s was not denied: %+v, %v", name, outcome, err)
		}
	}
}
