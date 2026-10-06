package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		Store: db, ChatProvider: provider, Runner: runner, ProviderName: "fixture", Model: "fixture",
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
	trace := m06ToolTraceOf(t, root, sessionID)
	result := trace.resultOf(t, "mcp__fixture__echo")
	if result.Error != "" || result.Result != "hello" {
		t.Fatalf("MCP tool result=%+v", result)
	}
	if messages := provider.roundMessages(1); len(messages) == 0 || messages[0].Content == "" {
		t.Fatal("first model round did not receive context")
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
