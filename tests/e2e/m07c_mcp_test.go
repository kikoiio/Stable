package e2e

import (
	"bytes"
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

func TestM07CMCPServiceRestartReplaysLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	fixture := m07cFixtureBinary(t)
	db, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	type childService struct {
		cmd    *exec.Cmd
		stop   string
		output *bytes.Buffer
	}
	startChild := func(phase int) *childService {
		t.Helper()
		base := t.TempDir()
		readyPath := filepath.Join(base, "ready")
		stopPath := filepath.Join(base, "stop")
		cmd := exec.Command(os.Args[0], "-test.run=^TestM07CMCPRestartProcessHelper$")
		cmd.Env = append(os.Environ(),
			"M07C_MCP_HELPER=1", "M07C_ROOT="+root, "M07C_STATE="+statePath,
			"M07C_FIXTURE="+fixture, "M07C_SOCKET="+filepath.Join(base, fmt.Sprintf("service-%d.sock", phase)),
			"M07C_READY="+readyPath, "M07C_STOP="+stopPath,
		)
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(readyPath); err == nil {
				return &childService{cmd: cmd, stop: stopPath, output: output}
			}
			if cmd.ProcessState != nil {
				t.Fatalf("MCP service child exited during startup: %s", output.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("MCP service child did not become ready: %s", output.String())
		return nil
	}
	stopChild := func(child *childService) {
		t.Helper()
		if err := os.WriteFile(child.stop, []byte("stop"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("MCP service child exit: %v\n%s", err, child.output.String())
		}
	}
	first := startChild(1)
	t.Cleanup(func() {
		if first != nil && first.cmd.ProcessState == nil {
			_ = first.cmd.Process.Kill()
			_ = first.cmd.Wait()
		}
	})
	created := m06Op(t, ctx, filepath.Join(filepath.Dir(first.stop), "service-1.sock"), conversation.ClientMsg{
		Op: "session_create", ProjectRoot: root,
	})
	var sessionID string
	for _, msg := range created {
		if msg.Type == "session" && msg.Session != nil {
			sessionID = msg.Session.ID
		}
	}
	if sessionID == "" {
		t.Fatalf("session_create response=%s", m06Dump(created))
	}
	msgs := m06Op(t, ctx, filepath.Join(filepath.Dir(first.stop), "service-1.sock"), conversation.ClientMsg{Op: "mcp_reload", SessionID: sessionID})
	if len(msgs) != 1 || msgs[0].MCPReport == nil || msgs[0].MCPReport.After != 1 {
		t.Fatalf("initial MCP reload=%s", m06Dump(msgs))
	}
	before, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeTypes []string
	for _, event := range before.Events {
		if event.Type == sessionlog.EventMCPReload || event.Type == sessionlog.EventMCPServer {
			beforeTypes = append(beforeTypes, event.Type)
		}
	}
	if len(beforeTypes) != 2 || beforeTypes[0] != sessionlog.EventMCPReload || beforeTypes[1] != sessionlog.EventMCPServer {
		t.Fatalf("MCP lifecycle events before restart=%v", beforeTypes)
	}

	// Stop the actual service process and start a fresh one over the same
	// session journal and SQLite store, as happens across a daemon restart.
	stopChild(first)
	first = nil
	second := startChild(2)
	t.Cleanup(func() {
		if second != nil && second.cmd.ProcessState == nil {
			_ = second.cmd.Process.Kill()
			_ = second.cmd.Wait()
		}
	})
	loaded := m06Op(t, ctx, filepath.Join(filepath.Dir(second.stop), "service-2.sock"), conversation.ClientMsg{
		Op: "session_load", ProjectRoot: root, SessionID: sessionID,
	})
	if len(loaded) != 1 || loaded[0].Transcript == nil {
		t.Fatalf("session_load after restart=%s", m06Dump(loaded))
	}
	var afterTypes []string
	for _, event := range loaded[0].Transcript.Events {
		if event.Type == sessionlog.EventMCPReload || event.Type == sessionlog.EventMCPServer {
			afterTypes = append(afterTypes, event.Type)
		}
	}
	if fmt.Sprint(afterTypes) != fmt.Sprint(beforeTypes) {
		t.Fatalf("MCP lifecycle events changed after restart: before=%v after=%v", beforeTypes, afterTypes)
	}
	var reload sessionlog.MCPReload
	var server sessionlog.MCPServer
	for _, event := range loaded[0].Transcript.Events {
		switch event.Type {
		case sessionlog.EventMCPReload:
			m06Decode(t, event.Data, &reload)
		case sessionlog.EventMCPServer:
			m06Decode(t, event.Data, &server)
		}
	}
	if reload.Trigger != "manual" || reload.After != 1 || server.Name != "fixture" || server.State != "connected" {
		t.Fatalf("replayed MCP lifecycle payloads: reload=%+v server=%+v", reload, server)
	}
	stopChild(second)
	second = nil
}

// TestM07CMCPRestartProcessHelper is invoked in a separate OS process by the
// restart e2e test. It keeps the service alive until the parent asks it to
// stop, so the next phase can prove recovery across a real process boundary.
func TestM07CMCPRestartProcessHelper(t *testing.T) {
	if os.Getenv("M07C_MCP_HELPER") != "1" {
		return
	}
	db, err := store.Open(os.Getenv("M07C_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := mcp.NewManager([]appconfig.MCPServerConfig{{Name: "fixture", Command: os.Getenv("M07C_FIXTURE")}}, "")
	if err = manager.ConnectAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ChatProvider: &m06Agent{}, ProjectRoot: os.Getenv("M07C_ROOT"),
		SocketPath: os.Getenv("M07C_SOCKET"), PollEvery: 100 * time.Millisecond, MCP: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err = os.WriteFile(os.Getenv("M07C_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	stopPath := os.Getenv("M07C_STOP")
	for {
		if _, err = os.Stat(stopPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
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
