package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
)

type coordinatorExecutorGateProbe struct{ calls int }

func (g *coordinatorExecutorGateProbe) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	g.calls++
	return permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "probe"}, nil
}

type coordinatorExecutorHookProbe struct{ pre, post int }

func (h *coordinatorExecutorHookProbe) PreToolUseRun(context.Context, agent.ParentRun, string, string, map[string]any) (bool, string, string) {
	h.pre++
	return false, "", ""
}

func (h *coordinatorExecutorHookProbe) PostToolUseRun(context.Context, agent.ParentRun, string, string, map[string]any, string) {
	h.post++
}

type coordinatorExecutorMCPProbe struct{ resolve, call int }

func (m *coordinatorExecutorMCPProbe) CallTool(context.Context, string, string, map[string]any) (string, bool, error) {
	m.call++
	return "unexpected", false, nil
}
func (*coordinatorExecutorMCPProbe) EagerSchemas() []execution.MCPToolSchema  { return nil }
func (*coordinatorExecutorMCPProbe) DispatchTools() []execution.MCPToolSchema { return nil }
func (*coordinatorExecutorMCPProbe) InputSchema(string, string) (map[string]any, bool) {
	return nil, false
}
func (m *coordinatorExecutorMCPProbe) ResolveTarget(string) (string, string, error) {
	m.resolve++
	return "fixture", "erase", nil
}
func (*coordinatorExecutorMCPProbe) Instructions() string { return "" }

func TestCoordinatorExecutorDeniesForgedDirectMCPInvocation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := ensureCoordinatorTestDir(root); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID:           "coordinator-direct-mcp",
		Work:            agent.WorkRef{Kind: agent.WorkSession, SessionID: "0123456789abcdef0123456789abcdef"},
		TeamCoordinator: true,
	}
	authority := permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID,
		AllowedRoot: root, FormalRoot: root, CandidateRoot: filepath.Join(root, ".candidate"),
		Mode: permission.ModeDefault,
	}
	request.PermissionBounds, _ = json.Marshal(authority)
	gate, hook, mcp := &coordinatorExecutorGateProbe{}, &coordinatorExecutorHookProbe{}, &coordinatorExecutorMCPProbe{}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: gate, MCP: mcp,
	}, execution.WithHookRunner(hook))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := executor.Execute(t.Context(), llm.ToolUse{
		ID: "forged-mcp", Name: "mcp__fixture__erase", Arguments: []byte(`{"path":"project"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied {
		t.Fatalf("direct MCP invocation outcome = %+v, want denied", outcome)
	}
	if gate.calls != 0 || hook.pre != 0 || hook.post != 0 || mcp.resolve != 0 || mcp.call != 0 {
		t.Fatalf("denied coordinator call reached downstream effects: gate=%d hook=%+v MCP=%+v", gate.calls, hook, mcp)
	}
}

func TestCoordinatorExecutorDeniesForgedNonTeamToolsBeforeSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := ensureCoordinatorTestDir(root); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID:           "coordinator-direct-tools",
		Work:            agent.WorkRef{Kind: agent.WorkSession, SessionID: "0123456789abcdef0123456789abcdef"},
		TeamCoordinator: true,
	}
	authority := permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID,
		AllowedRoot: root, FormalRoot: root, CandidateRoot: filepath.Join(root, ".candidate"),
		Mode: permission.ModeDefault,
	}
	request.PermissionBounds, _ = json.Marshal(authority)
	gate, hook, mcp := &coordinatorExecutorGateProbe{}, &coordinatorExecutorHookProbe{}, &coordinatorExecutorMCPProbe{}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: gate, MCP: mcp,
	}, execution.WithHookRunner(hook))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"read_file", "write_file", "glob", "grep", "command", "fetch_url",
		"run_agent", "delegate_tasks", "todo_write", "task_update",
	} {
		outcome, err := executor.Execute(t.Context(), llm.ToolUse{
			ID: "forged-" + name, Name: name, Arguments: []byte(`{"path":"project","command":"true"}`),
		})
		if err != nil || outcome.Status != agent.ToolDenied {
			t.Fatalf("direct coordinator call %s = %+v, %v; want denied", name, outcome, err)
		}
	}
	if gate.calls != 0 || hook.pre != 0 || hook.post != 0 || mcp.resolve != 0 || mcp.call != 0 {
		t.Fatalf("denied coordinator calls reached downstream effects: gate=%d hook=%+v MCP=%+v", gate.calls, hook, mcp)
	}
}

func ensureCoordinatorTestDir(path string) error {
	return os.MkdirAll(path, 0700)
}
