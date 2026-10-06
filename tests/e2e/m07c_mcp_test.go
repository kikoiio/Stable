package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/mcp"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type m07cAllowGate struct{}

func (m07cAllowGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "fixture approval"}, nil
}

func m07cFixtureBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcp-fixture")
	cmd := exec.Command("go", "build", "-o", bin, "../../internal/mcp/mcptest")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build MCP fixture: %v\n%s", err, output)
	}
	return bin
}

func m07cNewService(t *testing.T, root string, db *store.Store, provider *m06Agent, manager *mcp.Manager) *m06Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate:        m07cAllowGate{},
		Approvals:   db,
		Candidates:  db,
		SessionRoot: root,
		MCP:         manager,
		Now:         time.Now,
	}, execution.WithMCPCaller(manager))
	runner := agent.NewRunner(provider, agent.RunnerOptions{
		ExecutorFactory: factory,
		ToolSchemas: []llm.ToolSchema{
			{Name: "mcp_call", Description: "dispatch", InputSchema: map[string]any{"type": "object"}},
			{Name: "tool_search", Description: "search", InputSchema: map[string]any{"type": "object"}},
		},
		MaxRetries: -1,
	})
	socket := filepath.Join(filepath.Dir(root), fmt.Sprintf("chat-m07c-%d.sock", time.Now().UnixNano()))
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ChatProvider: provider, Runner: runner, ExecutorFactory: factory, ProviderName: "fixture", Model: "fixture",
		ProjectRoot: root, SocketPath: socket, PollEvery: 100 * time.Millisecond, MCP: manager,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	env := &m06Env{root: root, db: db, agent: provider, socket: socket, svc: svc, cancel: cancel}
	t.Cleanup(func() {
		svc.Close()
		cancel()
		manager.Shutdown()
	})
	return env
}

func TestM07CMCPServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	fixture := m07cFixtureBinary(t)
	manager := mcp.NewManager([]appconfig.MCPServerConfig{{Name: "fixture", Command: fixture, Env: map[string]string{"MCP_TEST_INSTRUCTIONS": "Use echo for fixture text."}}}, "")
	if err := manager.ConnectAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(manager.EagerSchemas()) != 1 {
		t.Fatalf("eager schemas=%v", manager.EagerSchemas())
	}
	provider := &m06Agent{respond: func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("mcp-call-1", "mcp__fixture__echo", map[string]any{"text": "hello"})
		}
		return m06TextRound("done")
	}}
	env := m07cNewService(t, root, db, provider, manager)
	sessionID := m06SessionCreate(t, ctx, env)
	stream := m06StartRunWithText(t, ctx, env, sessionID, "mcp-run-1", "echo")
	stream.waitOutcome(t, 8*time.Second)
	firstRound := provider.roundMessages(1)
	firstHasInstructions := false
	for _, message := range firstRound {
		firstHasInstructions = firstHasInstructions || strings.Contains(message.Content, "MCP Server Instructions")
	}
	if !firstHasInstructions {
		t.Fatalf("first model round did not receive MCP instructions: %+v", firstRound)
	}
	trace := m06ToolTraceOf(t, root, sessionID)
	result := trace.resultOf(t, "mcp__fixture__echo")
	if result.Error != "" || result.Result != "hello" {
		t.Fatalf("MCP tool result=%+v", result)
	}
	provider.reset()
	provider.respond = func(_ int, _ llm.Request) []llm.Event { return m06TextRound("second run") }
	second := m06StartRunWithText(t, ctx, env, sessionID, "mcp-run-2", "again")
	second.waitOutcome(t, 8*time.Second)
	for _, message := range provider.roundMessages(1) {
		if strings.Contains(message.Content, "MCP Server Instructions") {
			t.Fatal("MCP instructions were injected more than once in one session")
		}
	}
	listed := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "mcp_list", SessionID: sessionID})
	if len(listed) != 1 || listed[0].MCPList == nil || len(listed[0].MCPList.Servers) != 1 {
		t.Fatalf("mcp_list=%s", m06Dump(listed))
	}
	reloaded := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "mcp_reload", SessionID: sessionID})
	if len(reloaded) != 1 || reloaded[0].MCPReport == nil {
		t.Fatalf("mcp_reload=%s", m06Dump(reloaded))
	}
	replay, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	seenReload, seenServer := false, false
	for _, event := range replay.Events {
		seenReload = seenReload || event.Type == sessionlog.EventMCPReload
		seenServer = seenServer || event.Type == sessionlog.EventMCPServer
	}
	if !seenReload || !seenServer {
		t.Fatalf("MCP lifecycle events missing: reload=%v server=%v", seenReload, seenServer)
	}
	_ = os.Remove(fixture)
}

func TestM07CMCPDispatchAndUnknownTarget(t *testing.T) {
	t.Setenv("STABLE_MCP_LOADING", "dispatch")
	ctx := context.Background()
	root, db := m06NewProject(t)
	fixture := m07cFixtureBinary(t)
	manager := mcp.NewManager([]appconfig.MCPServerConfig{{Name: "fixture", Command: fixture}}, "")
	if err := manager.ConnectAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(manager.EagerSchemas()) != 0 || len(manager.DispatchTools()) != 1 {
		t.Fatalf("dispatch tiers eager=%v dispatch=%v", manager.EagerSchemas(), manager.DispatchTools())
	}
	provider := &m06Agent{respond: func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("search-1", "tool_search", map[string]any{"query": "echo"})
		case 2:
			return m06ToolRound("call-1", "mcp_call", map[string]any{
				"server": "fixture", "tool": "echo", "arguments": map[string]any{"text": "dispatch"},
			})
		default:
			return m06TextRound("done")
		}
	}}
	env := m07cNewService(t, root, db, provider, manager)
	sessionID := m06SessionCreate(t, ctx, env)
	stream := m06StartRunWithText(t, ctx, env, sessionID, "mcp-dispatch-1", "dispatch")
	stream.waitOutcome(t, 8*time.Second)
	trace := m06ToolTraceOf(t, root, sessionID)
	if result := trace.resultOf(t, "tool_search"); result.Error != "" || !strings.Contains(fmt.Sprint(result.Result), "mcp__fixture__echo") {
		t.Fatalf("tool_search result=%+v", result)
	}
	if result := trace.resultOf(t, "mcp_call"); result.Error != "" || result.Result != "dispatch" {
		t.Fatalf("mcp_call result=%+v", result)
	}

	provider.reset()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("unknown-1", "mcp_call", map[string]any{"server": "fixture", "tool": "missing"})
		}
		return m06TextRound("done")
	}
	stream = m06StartRunWithText(t, ctx, env, sessionID, "mcp-dispatch-2", "unknown")
	stream.waitOutcome(t, 8*time.Second)
	unknown := m06ToolTraceOf(t, root, sessionID).results["unknown-1"]
	if unknown.Error == "" || !strings.Contains(fmt.Sprint(unknown.Result), "mcp__fixture__echo") {
		t.Fatalf("unknown MCP target result=%+v", unknown)
	}
}

func TestM07CMCPFailureIsolation(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	fixture := m07cFixtureBinary(t)
	manager := mcp.NewManager([]appconfig.MCPServerConfig{
		{Name: "good", Command: fixture},
		{Name: "gone", Command: fixture, Env: map[string]string{"MCP_TEST_MODE": "exit"}},
	}, "")
	if err := manager.ConnectAll(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := manager.Status()
	if len(statuses) != 2 || statuses[0].State != "connected" || statuses[1].State != "reload-failed" {
		t.Fatalf("failure isolation statuses=%+v", statuses)
	}
	provider := &m06Agent{respond: func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("good-1", "mcp__good__echo", map[string]any{"text": "isolated"})
		}
		return m06TextRound("done")
	}}
	env := m07cNewService(t, root, db, provider, manager)
	sessionID := m06SessionCreate(t, ctx, env)
	listed := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "mcp_list", SessionID: sessionID})
	if len(listed) != 1 || listed[0].MCPList == nil || len(listed[0].MCPList.Servers) != 2 {
		t.Fatalf("failure isolation list=%s", m06Dump(listed))
	}
	stream := m06StartRunWithText(t, ctx, env, sessionID, "mcp-failure-isolation", "good server")
	stream.waitOutcome(t, 8*time.Second)
	result := m06ToolTraceOf(t, root, sessionID).resultOf(t, "mcp__good__echo")
	if result.Error != "" || result.Result != "isolated" {
		t.Fatalf("healthy server result=%+v", result)
	}
}

func TestM07CMCPProjectMtimeReloadsAndRefreshesTools(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	fixture := m07cFixtureBinary(t)
	configDir := filepath.Join(root, ".stable")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "mcp.yaml")
	manager := mcp.NewManager(nil, configPath)
	if err := manager.ConnectAll(ctx); err != nil {
		t.Fatal(err)
	}
	provider := &m06Agent{respond: func(_ int, _ llm.Request) []llm.Event { return m06TextRound("done") }}
	env := m07cNewService(t, root, db, provider, manager)
	sessionID := m06SessionCreate(t, ctx, env)
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("- name: fixture\n  command: %s\n", fixture)), 0600); err != nil {
		t.Fatal(err)
	}
	listed := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "mcp_list", SessionID: sessionID})
	if len(listed) != 1 || listed[0].MCPList == nil || len(listed[0].MCPList.Servers) != 1 {
		t.Fatalf("mtime reload list=%s", m06Dump(listed))
	}
	stream := m06StartRunWithText(t, ctx, env, sessionID, "mcp-mtime-run", "inspect tools")
	stream.waitOutcome(t, 8*time.Second)
	var foundEager bool
	for _, tool := range provider.roundTools(1) {
		if tool.Name == "mcp__fixture__echo" {
			foundEager = true
			break
		}
	}
	if !foundEager {
		t.Fatalf("runner tools after mtime reload=%v", provider.roundTools(1))
	}
	if err := os.WriteFile(configPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "mcp_reload", SessionID: sessionID})
	if len(reloaded) != 1 || reloaded[0].MCPReport == nil || reloaded[0].MCPReport.Before != 1 || reloaded[0].MCPReport.After != 0 {
		t.Fatalf("manual removal report=%s", m06Dump(reloaded))
	}
	if len(manager.Status()) != 0 {
		t.Fatalf("servers after removal=%+v", manager.Status())
	}
	if len(m06EventsOfType(t, root, sessionID, sessionlog.EventMCPReload)) < 2 {
		t.Fatal("expected automatic and manual MCP reload events")
	}
}
