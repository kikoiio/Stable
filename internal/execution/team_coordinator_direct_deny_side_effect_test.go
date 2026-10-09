package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

func TestCoordinatorRejectsForgedDirectCallsBeforeExecutionSideEffects(t *testing.T) {
	// Bypass mode would otherwise allow these operations through the permission
	// gate. Coordinator authorization is a separate, static run boundary.
	authority := m06Authority(t, t.TempDir(), permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	mcp := newFakeMCPCaller()
	tasks := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{ID: "task-1", Status: agent.DelegationQueued}}
	delegator := &delegationStub{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate:       gate,
		HookRunner: hooks,
		MCP:        mcp,
		Now:        time.Now,
	}, WithAgentTaskService(tasks), WithDelegator(delegator, testDelegationProvider{}))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	// Cover the project inspection/mutation and recursive/MCP paths with
	// syntactically valid payloads, so a rejection cannot be attributed to
	// parsing or missing optional dependencies.
	for _, call := range []llm.ToolUse{
		m06Call("read_file", `{"file_path":"secret.txt"}`),
		m06Call("glob", `{"pattern":"**/*"}`),
		m06Call("grep", `{"pattern":"secret"}`),
		m06Call("write_file", `{"file_path":"created.txt","content":"forged"}`),
		m06Call("command", `{"command":"touch forged"}`),
		m06Call("mcp_call", `{"tool":"github__create_issue","input":{"title":"forged"}}`),
		m06Call("mcp__github__create_issue", `{"title":"forged"}`),
		m06Call("run_agent", `{"agent_name":"explore","instruction":"recurse"}`),
		m06Call("task_output", `{"task_id":"task-1"}`),
		m06Call("task_stop", `{"task_id":"task-1"}`),
		m06Call("delegate_tasks", `{"tasks":[{"id":"nested","name":"nested","instruction":"recurse"}]}`),
		m06Call("task_update", `{"taskId":"todo-1"}`),
		m06Call("http_request", `{"url":"https://example.invalid"}`),
	} {
		outcome, execErr := executor.Execute(context.Background(), call)
		if execErr != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
			t.Fatalf("coordinator direct call %s escaped static mode filter: outcome=%+v err=%v", call.Name, outcome, execErr)
		}
	}

	if gate.calls != 0 {
		t.Fatalf("forged coordinator calls reached permission gate %d times", gate.calls)
	}
	if hooks.preCalls != 0 || hooks.postCalls != 0 {
		t.Fatalf("forged coordinator calls reached hooks pre/post=%d/%d", hooks.preCalls, hooks.postCalls)
	}
	if len(mcp.calls) != 0 || len(mcp.queries) != 0 {
		t.Fatalf("forged coordinator calls reached MCP resolver/caller: queries=%v calls=%v", mcp.queries, mcp.calls)
	}
	if tasks.calls != 0 {
		t.Fatalf("forged coordinator calls reached named-task service %d times", tasks.calls)
	}
	if len(delegator.tasks) != 0 {
		t.Fatalf("forged coordinator calls reached recursive delegator: %+v", delegator.tasks)
	}
}
