package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/workspace"
)

type coordinatorToolSearchMCPProbe struct {
	*fakeMCPCaller
	dispatchCalls int
}

func (p *coordinatorToolSearchMCPProbe) DispatchTools() []MCPToolSchema {
	p.dispatchCalls++
	return p.fakeMCPCaller.DispatchTools()
}

func TestCoordinatorDirectToolSearchIsDeniedBeforeDiscoverySideEffects(t *testing.T) {
	authority := m06Authority(t, t.TempDir(), permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	mcp := &coordinatorToolSearchMCPProbe{fakeMCPCaller: newFakeMCPCaller()}
	teamHost := &agent.TeamToolHost{}
	teamHostCalls := 0
	teamHost.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		teamHostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded}, nil
	})
	lifecycle := NewWorkspaceLifecycleToolHost()
	lifecycleCalls := 0
	lifecycle.Bind(func(_ context.Context, _ agent.ExecutionRequest, _ *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
		lifecycleCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded}, nil
	})
	delegator := &coordinatorManagementDelegatorProbe{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate: gate, HookRunner: hooks, MCP: mcp, Now: time.Now,
	}, WithTeamToolHost(teamHost), WithDelegator(delegator, nil), WithWorkspaceLifecycleToolHost(lifecycle))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.Execute(context.Background(), llm.ToolUse{
		ID: "forged-tool-search", Name: "tool_search",
		Arguments: []byte(`{"query":"private repository tools"}`),
	})
	if err != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
		t.Fatalf("coordinator direct tool_search = %+v, %v; want static coordinator denial", outcome, err)
	}
	if mcp.dispatchCalls != 0 || len(mcp.queries) != 0 || len(mcp.calls) != 0 {
		t.Fatalf("denied tool_search reached MCP discovery/caller: dispatch=%d queries=%v calls=%v", mcp.dispatchCalls, mcp.queries, mcp.calls)
	}
	if gate.calls != 0 || hooks.preCalls != 0 || hooks.postCalls != 0 || teamHostCalls != 0 || lifecycleCalls != 0 || delegator.calls != 0 {
		t.Fatalf("denied tool_search reached downstream effects: gate=%d hooks=%d/%d team_host=%d lifecycle=%d delegator=%d", gate.calls, hooks.preCalls, hooks.postCalls, teamHostCalls, lifecycleCalls, delegator.calls)
	}
}
