package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
)

type hookAgentTestProvider struct{}

func (hookAgentTestProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	return make(chan llm.Event), make(chan error)
}

type hookAgentDelegator struct {
	parent   agent.ParentRun
	task     agent.DelegationTask
	result   agent.DelegationResult
	err      error
	called   bool
	deadline time.Time
	entered  chan struct{}
	release  chan struct{}
}

func (d *hookAgentDelegator) RunBatch(context.Context, agent.ParentRun, []agent.DelegationTask) ([]agent.DelegationResult, error) {
	return nil, errors.New("unexpected RunBatch")
}

func (d *hookAgentDelegator) RunTask(ctx context.Context, parent agent.ParentRun, task agent.DelegationTask) (agent.DelegationResult, error) {
	d.parent, d.task, d.called = parent, task, true
	d.deadline, _ = ctx.Deadline()
	if d.entered != nil {
		close(d.entered)
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return agent.DelegationResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}, nil
		}
	}
	return d.result, d.err
}

func TestHookAgentRejectRemainsStatic(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationSucceeded, Summary: "allow"}}
	gate := newHookAgentGate(t, `hooks:
  - id: fixed-deny
    event: pre_tool_use
    reject: true
    async: true
    action:
      type: agent
      message: "inspect this call"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-deny", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-deny"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	rejected, hookID, _ := gate.PreToolUseRun(context.Background(), parent, "session-deny", "read_file", map[string]any{"path": "a.txt"})
	if !rejected || hookID != "fixed-deny" {
		t.Fatalf("model output changed static rejection: rejected=%v hook=%q", rejected, hookID)
	}
}

func newHookAgentGate(t *testing.T, config string, delegator agent.Delegator) *HookGate {
	t.Helper()
	root := t.TempDir()
	hookPath := filepath.Join(root, "hooks.yaml")
	if err := os.WriteFile(hookPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{Delegator: delegator, ProjectRoot: root, ProviderCredential: "secret-value"}}
	gate := NewHookGate(service, hookPath, "")
	gate.Bind(service)
	t.Cleanup(gate.Close)
	return gate
}

func TestHookAgentUsesMessageContextAndStaticReject(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{
		ChildRunID: "child-1", Status: agent.DelegationSucceeded, Summary: "the model recommends reject",
	}}
	gate := newHookAgentGate(t, `hooks:
  - id: audit
    event: pre_tool_use
    reject: false
    action:
      type: agent
      message: "review this tool call"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-1", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: "session-1", GoalID: "goal-1"},
		Provider: hookAgentTestProvider{}, ProviderName: "fake", Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	rejected, hookID, message := gate.PreToolUseRun(context.Background(), parent, "session-1", "read_file", map[string]any{"path": "README.md", "note": "secret-value"})
	if rejected || hookID != "" || message != "" {
		t.Fatalf("model summary changed static reject result: rejected=%v hook=%q message=%q", rejected, hookID, message)
	}
	if !delegator.called || delegator.parent.RunID != parent.RunID || delegator.parent.Work != parent.Work {
		t.Fatalf("parent scope was not forwarded: %+v", delegator.parent)
	}
	if !strings.Contains(delegator.task.Instruction, "review this tool call") || !strings.Contains(delegator.task.Instruction, `"tool_name":"read_file"`) || !strings.Contains(delegator.task.Instruction, `"path":"README.md"`) {
		t.Fatalf("instruction lacks prompt or event fields: %s", delegator.task.Instruction)
	}
	if strings.Contains(delegator.task.Instruction, "secret-value") {
		t.Fatal("provider credential was included in the child instruction")
	}
	if strings.Contains(delegator.task.Instruction, "parent conversation") {
		t.Fatal("hook agent received unrequested parent conversation history")
	}
	if notice := gate.DrainNotifications("session-1"); !strings.Contains(notice, "the model recommends reject") {
		t.Fatalf("hook summary did not enter the notification queue: %q", notice)
	}
}

func TestHookAgentCommandFallbackAndErrorReject(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{
		Status: agent.DelegationFailed, Error: "child failed",
	}}
	gate := newHookAgentGate(t, `hooks:
  - id: fallback
    event: run_start
    action:
      type: agent
      command: "legacy prompt"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-2", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-2"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	gate.RunStartRun(context.Background(), parent, "session-2", "start intent")
	if !delegator.called || !strings.Contains(delegator.task.Instruction, "legacy prompt") {
		t.Fatalf("command fallback was not used: called=%v task=%+v", delegator.called, delegator.task)
	}
	if notice := gate.DrainNotifications("session-2"); !strings.Contains(notice, "child failed") {
		t.Fatalf("failure reason was not queued: %q", notice)
	}
}

func TestHookAgentOnErrorRejectBlocksPreToolUse(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationFailed, Error: "provider unavailable"}}
	gate := newHookAgentGate(t, `hooks:
  - id: reject-on-error
    event: pre_tool_use
    on_error: reject
    action:
      type: agent
      message: "inspect call"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-error", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-error"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	rejected, hookID, message := gate.PreToolUseRun(context.Background(), parent, "session-error", "read_file", map[string]any{"path": "a"})
	if !rejected || hookID != "reject-on-error" || !strings.Contains(message, "provider unavailable") {
		t.Fatalf("on_error reject did not block failed hook: rejected=%v hook=%q message=%q", rejected, hookID, message)
	}
}

func TestAsyncHookAgentReturnsBeforeChildCompletion(t *testing.T) {
	delegator := &hookAgentDelegator{
		result:  agent.DelegationResult{Status: agent.DelegationSucceeded, Summary: "async complete"},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	gate := newHookAgentGate(t, `hooks:
  - id: async-review
    event: post_tool_use
    async: true
    action:
      type: agent
      message: "review result"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-async", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-async"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	start := time.Now()
	gate.PostToolUseRun(context.Background(), parent, "session-async", "read_file", map[string]any{"path": "a"}, "result")
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("async hook blocked on child completion")
	}
	select {
	case <-delegator.entered:
	case <-time.After(time.Second):
		t.Fatal("async child did not start")
	}
	close(delegator.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if notice := gate.DrainNotifications("session-async"); strings.Contains(notice, "async complete") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("async result did not enter notification queue")
}

func TestHookAgentTimeoutIsCappedAndInputIsBounded(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationSucceeded}}
	parent := agent.ParentRun{
		RunID: "parent-3", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-3"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	result := (HookAgentCoordinator{Delegator: delegator}).Execute(context.Background(), HookAgentInvocation{
		Parent: parent, HookID: "timeout", Instruction: strings.Repeat("x", 65<<10),
		Timeout: time.Hour,
	})
	if delegator.called || result.Success || !strings.Contains(result.Output, "exceeds") {
		t.Fatalf("oversize input was not rejected before queueing: called=%v result=%+v", delegator.called, result)
	}
	delegator = &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationSucceeded}}
	result = (HookAgentCoordinator{Delegator: delegator}).Execute(context.Background(), HookAgentInvocation{
		Parent: parent, HookID: "timeout", Instruction: "small prompt", Timeout: time.Hour,
	})
	if !result.Success || delegator.deadline.IsZero() || time.Until(delegator.deadline) > hookAgentMaxDuration {
		t.Fatalf("agent timeout did not respect 3 minute cap: result=%+v deadline=%s", result, delegator.deadline)
	}
}
