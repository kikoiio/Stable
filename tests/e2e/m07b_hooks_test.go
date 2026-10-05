package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func m07bWriteHooks(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func m07bNewService(t *testing.T, root string, db *store.Store, provider *m06Agent, helperPath, userHooks string) *m06Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	askSink := conversation.NewAskAdapter(nil)
	todoProvider := conversation.NewTodoProvider(nil)
	planSink := conversation.NewPlanApprovalSink(nil)
	skillGate := conversation.NewSkillGate(nil, "", filepath.Join(root, ".stable", "skills"))
	hookGate := conversation.NewHookGate(nil, userHooks, filepath.Join(root, ".stable", "hooks.yaml"))
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate:         execution.StorePermissionGate{Store: db},
		Approvals:    db,
		Candidates:   db,
		HelperPath:   helperPath,
		SessionRoot:  root,
		QuestionSink: askSink,
		TodoProvider: todoProvider,
	}, execution.WithPlanSink(planSink), execution.WithSkillProvider(skillGate), execution.WithHookRunner(hookGate))
	runner := agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: m07aToolSchemas(), MaxRetries: -1})
	socket := filepath.Join(filepath.Dir(root), fmt.Sprintf("chat-m07b-%d.sock", time.Now().UnixNano()))
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ChatProvider: provider, Runner: runner,
		ProviderName: "fixture", Model: "fixture",
		ProjectRoot: root, SocketPath: socket, PollEvery: 200 * time.Millisecond,
		Skills: skillGate, Hooks: hookGate,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	askSink.Bind(svc)
	todoProvider.Bind(svc)
	planSink.Bind(svc)
	env := &m06Env{root: root, db: db, agent: provider, socket: socket, svc: svc, cancel: cancel}
	t.Cleanup(func() {
		svc.Close()
		cancel()
	})
	return env
}

func m07bHookEvents(t *testing.T, root, sessionID string) []sessionlog.HookFired {
	t.Helper()
	var out []sessionlog.HookFired
	for _, e := range m06EventsOfType(t, root, sessionID, sessionlog.EventHookFired) {
		var fired sessionlog.HookFired
		m06Decode(t, e.Data, &fired)
		out = append(out, fired)
	}
	return out
}

func TestM07BHooksListRejectNotifyReloadAndRestart(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	userHooks := filepath.Join(t.TempDir(), "hooks.yaml")
	m07bWriteHooks(t, userHooks, `
hooks:
  - id: greet
    event: run_start
    action: {type: prompt, message: hello-user}
  - id: shared
    event: run_end
    action: {type: prompt, message: user-end}
`)
	m07bWriteHooks(t, filepath.Join(root, ".stable", "hooks.yaml"), `
hooks:
  - id: shared
    event: run_end
    action: {type: prompt, message: project-end}
  - id: block
    event: pre_tool_use
    reject: true
    if: "tool == command"
    action: {type: prompt, message: no-bash}
  - id: after
    event: post_tool_use
    if: "tool == task_list"
    action: {type: prompt, message: listed}
  - id: once
    event: run_start
    once: true
    action: {type: prompt, message: once-only}
  - id: http
    event: run_start
    action: {type: http, url: "https://example.com"}
`)
	helper := m06HelperPath(t)
	provider := &m06Agent{respond: func(int, llm.Request) []llm.Event { return m06TextRound("first") }}
	env := m07bNewService(t, root, db, provider, helper, userHooks)
	sessionID := m06SessionCreate(t, ctx, env)

	listed := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "hooks_list", SessionID: sessionID})
	if len(listed) == 0 || listed[0].HookList == nil {
		t.Fatalf("hooks_list: %s", m06Dump(listed))
	}
	ids := map[string]string{}
	for _, h := range listed[0].HookList.Hooks {
		ids[h.ID] = h.Source
	}
	if ids["greet"] != "user" || ids["shared"] != "project" || ids["block"] != "project" {
		t.Fatalf("merge sources=%v", ids)
	}

	stream := m06StartRunWithText(t, ctx, env, sessionID, "run-1", "hello")
	stream.waitOutcome(t, 8*time.Second)
	kinds := map[string]int{}
	for _, h := range m07bHookEvents(t, root, sessionID) {
		kinds[h.HookID+"/"+h.Event]++
		if h.HookID == "http" && h.Success {
			t.Fatalf("http action succeeded: %+v", h)
		}
	}
	if kinds["greet/run_start"] != 1 || kinds["once/run_start"] != 1 || kinds["shared/run_end"] != 1 {
		t.Fatalf("first run hooks=%v", kinds)
	}

	provider.reset()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("c1", "command", map[string]any{"command": "echo hi"})
		}
		return m06TextRound("blocked")
	}
	stream = m06StartRunWithText(t, ctx, env, sessionID, "run-2", "run cmd")
	stream.waitOutcome(t, 8*time.Second)
	text := m06ResultText(t, m06ToolTraceOf(t, root, sessionID).resultOf(t, "command"))
	if !strings.Contains(text, "Blocked by hook") || !strings.Contains(text, "block") {
		t.Fatalf("reject result=%q", text)
	}

	provider.reset()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("t1", "task_list", map[string]any{})
		}
		return m06TextRound("listed-ok")
	}
	stream = m06StartRunWithText(t, ctx, env, sessionID, "run-3", "list tasks")
	stream.waitOutcome(t, 8*time.Second)

	provider.reset()
	provider.respond = func(int, llm.Request) []llm.Event { return m06TextRound("second") }
	stream = m06StartRunWithText(t, ctx, env, sessionID, "run-4", "again")
	stream.waitOutcome(t, 8*time.Second)
	joined := ""
	for _, m := range provider.roundMessages(1) {
		joined += m.Content
	}
	if !strings.Contains(joined, "listed") || !strings.Contains(joined, "hook-notification") {
		t.Fatalf("missing post_tool_use notice in next run: %s", joined)
	}
	onceCount := 0
	for _, h := range m07bHookEvents(t, root, sessionID) {
		if h.HookID == "once" {
			onceCount++
		}
	}
	if onceCount != 1 {
		t.Fatalf("once fired %d times", onceCount)
	}

	reload := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "hooks_reload", SessionID: sessionID})
	if len(reload) == 0 || reload[0].HookReport == nil {
		t.Fatalf("reload: %s", m06Dump(reload))
	}

	before := m07bHookEvents(t, root, sessionID)
	env.svc.Close()
	env.cancel()
	provider2 := &m06Agent{respond: func(int, llm.Request) []llm.Event { return m06TextRound("restarted") }}
	_ = m07bNewService(t, root, db, provider2, helper, userHooks)
	replayed := m07bHookEvents(t, root, sessionID)
	if len(replayed) != len(before) {
		t.Fatalf("restart lost hook events: before=%d after=%d", len(before), len(replayed))
	}
}
