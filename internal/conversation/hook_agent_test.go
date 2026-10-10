package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
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
	calls    int
	deadline time.Time
	entered  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	tasks    []agent.DelegationTask
}

func (d *hookAgentDelegator) RunBatch(context.Context, agent.ParentRun, []agent.DelegationTask) ([]agent.DelegationResult, error) {
	return nil, errors.New("unexpected RunBatch")
}

func (d *hookAgentDelegator) RunTask(ctx context.Context, parent agent.ParentRun, task agent.DelegationTask) (agent.DelegationResult, error) {
	d.parent, d.task, d.called = parent, task, true
	d.calls++
	d.tasks = append(d.tasks, task)
	d.deadline, _ = ctx.Deadline()
	if d.entered != nil {
		close(d.entered)
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			if d.canceled != nil {
				close(d.canceled)
			}
			return agent.DelegationResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}, nil
		}
	}
	return d.result, d.err
}

func TestHookAgentCoversSessionAndGoalLifecycleEvents(t *testing.T) {
	for _, kind := range []agent.WorkKind{agent.WorkSession, agent.WorkGoal} {
		t.Run(string(kind), func(t *testing.T) {
			delegator := &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationSucceeded}}
			gate := newHookAgentGate(t, `hooks:
  - id: start
    event: run_start
    action: {type: agent, message: inspect-start}
  - id: pre
    event: pre_tool_use
    action: {type: agent, message: inspect-pre}
  - id: post
    event: post_tool_use
    action: {type: agent, message: inspect-post}
  - id: end
    event: run_end
    action: {type: agent, message: inspect-end}
`, delegator)
			work := agent.WorkRef{Kind: kind, SessionID: "session-lifecycle"}
			if kind == agent.WorkGoal {
				work.GoalID, work.WorkItemID = "goal-lifecycle", "item-lifecycle"
			}
			parent := agent.ParentRun{
				RunID: "parent-lifecycle", Work: work,
				Provider: hookAgentTestProvider{}, ProviderName: "fake", Model: "fake-model", ProjectRoot: t.TempDir(),
			}
			gate.RunStartRun(context.Background(), parent, work.SessionID, "start event")
			if rejected, _, _ := gate.PreToolUseRun(context.Background(), parent, work.SessionID, "read_file", map[string]any{"path": "a"}); rejected {
				t.Fatal("pre_tool_use was rejected unexpectedly")
			}
			gate.PostToolUseRun(context.Background(), parent, work.SessionID, "read_file", map[string]any{"path": "a"}, "result")
			gate.RunEndRun(context.Background(), parent, work.SessionID, string(agent.RunCompleted), "done")
			if delegator.calls != 4 {
				t.Fatalf("expected all four lifecycle hooks, got %d calls", delegator.calls)
			}
			for i, event := range []string{"run_start", "pre_tool_use", "post_tool_use", "run_end"} {
				if !strings.Contains(delegator.tasks[i].Instruction, `"event":"`+event+`"`) {
					t.Fatalf("task %d does not contain %s: %s", i, event, delegator.tasks[i].Instruction)
				}
			}
			if delegator.parent.Work != work {
				t.Fatalf("last event lost parent work scope: got=%+v want=%+v", delegator.parent.Work, work)
			}
		})
	}
}

func TestAsyncHookAgentSurvivesNormalParentRunCompletion(t *testing.T) {
	delegator := &hookAgentDelegator{
		result:  agent.DelegationResult{Status: agent.DelegationSucceeded, Summary: "async complete"},
		entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}),
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
		RunID: "parent-async-complete", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-async-complete"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	parentCtx, cancelParent := context.WithCancel(context.Background())
	gate.PostToolUseRun(parentCtx, parent, parent.Work.SessionID, "read_file", map[string]any{"path": "a"}, "result")
	select {
	case <-delegator.entered:
	case <-time.After(time.Second):
		t.Fatal("async child did not start")
	}
	cancelParent()
	gate.RunEndRun(context.Background(), parent, parent.Work.SessionID, string(agent.RunCompleted), "done")
	select {
	case <-delegator.canceled:
		t.Fatal("normal parent completion canceled the async child")
	case <-time.After(25 * time.Millisecond):
	}
	close(delegator.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if notice := gate.DrainNotifications(parent.Work.SessionID); strings.Contains(notice, "async complete") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("async child result was not delivered after parent completion")
}

func TestAsyncHookAgentCanceledWhenParentRunIsCancelled(t *testing.T) {
	delegator := &hookAgentDelegator{
		entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}),
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
		RunID: "parent-async-cancel", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-async-cancel"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	parentCtx, cancelParent := context.WithCancel(context.Background())
	gate.PostToolUseRun(parentCtx, parent, parent.Work.SessionID, "read_file", map[string]any{"path": "a"}, "result")
	select {
	case <-delegator.entered:
	case <-time.After(time.Second):
		t.Fatal("async child did not start")
	}
	cancelParent()
	gate.RunEndRun(context.Background(), parent, parent.Work.SessionID, string(agent.RunCancelled), "cancelled")
	select {
	case <-delegator.canceled:
	case <-time.After(time.Second):
		t.Fatal("parent run cancellation did not stop the async child")
	}
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

func TestHookAgentMessageTakesPriorityAndEmptyActionIsRejected(t *testing.T) {
	delegator := &hookAgentDelegator{result: agent.DelegationResult{Status: agent.DelegationSucceeded}}
	gate := newHookAgentGate(t, `hooks:
  - id: priority
    event: run_start
    action:
      type: agent
      message: "preferred instruction"
      command: "legacy fallback"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-priority", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-priority"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	gate.RunStartRun(context.Background(), parent, parent.Work.SessionID, "start")
	if !delegator.called || !strings.Contains(delegator.task.Instruction, "preferred instruction") || strings.Contains(delegator.task.Instruction, "legacy fallback") {
		t.Fatalf("message did not take priority over command: %+v", delegator.task)
	}

	invalid := newHookAgentGate(t, `hooks:
  - id: missing-instruction
    event: run_start
    action: {type: agent}
`, nil)
	if rejections := strings.Join(invalid.Rejections(), "\n"); !strings.Contains(rejections, "agent action requires message or command") {
		t.Fatalf("empty agent action was not rejected during config loading: %q", rejections)
	}
}

func TestHookAgentPreToolUseWaitsEvenWhenConfiguredAsync(t *testing.T) {
	delegator := &hookAgentDelegator{
		result:  agent.DelegationResult{Status: agent.DelegationSucceeded},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	gate := newHookAgentGate(t, `hooks:
  - id: synchronous-gate
    event: pre_tool_use
    async: true
    action:
      type: agent
      message: "inspect before tool"
`, delegator)
	parent := agent.ParentRun{
		RunID: "parent-pre-sync", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-pre-sync"},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: t.TempDir(),
	}
	result := make(chan struct {
		rejected bool
		id       string
		message  string
	}, 1)
	go func() {
		rejected, id, message := gate.PreToolUseRun(context.Background(), parent, parent.Work.SessionID, "read_file", map[string]any{"path": "a"})
		result <- struct {
			rejected bool
			id       string
			message  string
		}{rejected, id, message}
	}()
	select {
	case <-delegator.entered:
	case <-time.After(time.Second):
		t.Fatal("pre_tool_use child did not start")
	}
	select {
	case <-result:
		t.Fatal("pre_tool_use returned before its child completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(delegator.release)
	select {
	case got := <-result:
		if got.rejected || got.id != "" || got.message != "" {
			t.Fatalf("successful child unexpectedly rejected tool: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("pre_tool_use did not return after child completion")
	}
}

func TestHookAgentParentCarriesRunAuthorityAndReadOnlyTools(t *testing.T) {
	provider := hookAgentTestProvider{}
	bounds := json.RawMessage(`{"run_id":"run-authority","allowed_root":"/authorized","candidate_root":"/candidate"}`)
	svc := &Service{deps: Deps{
		ProjectRoot: "/workspace", ForkProvider: provider,
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}},
	}}
	request := agent.ExecutionRequest{
		RunID: "run-authority", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: "session", GoalID: "goal", WorkItemID: "item"},
		ProviderName: "fixture-provider", Model: "fixture-model", PermissionBounds: bounds,
	}
	parent := svc.hookAgentParent(request)
	var authority map[string]any
	if err := json.Unmarshal(parent.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	if parent.Provider != provider || parent.ProviderName != request.ProviderName || parent.Model != request.Model || parent.Work != request.Work || parent.ProjectRoot != "/authorized" || authority["allowed_root"] != "/authorized" {
		t.Fatalf("parent run authority was not carried into hook delegation: parent=%+v authority=%v", parent, authority)
	}
	if len(parent.ToolSchemas) != 1 || parent.ToolSchemas[0].Name != "read_file" {
		t.Fatalf("hook child did not receive the configured read-only schemas: %+v", parent.ToolSchemas)
	}
}

func TestHookAgentJournalRedactsBoundsAndAssociatesChildRun(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	delegator := &hookAgentDelegator{result: agent.DelegationResult{
		ChildRunID: "child-journal", Status: agent.DelegationSucceeded,
		Summary: "token-secret " + strings.Repeat("x", sessionlog.MaxHookOutput+10),
	}}
	hookPath := filepath.Join(root, "hooks.yaml")
	if err = os.WriteFile(hookPath, []byte(`hooks:
  - id: journal
    event: run_start
    action: {type: agent, message: "summarize"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{ProjectRoot: root, ProviderCredential: "token-secret", Delegator: delegator}}
	gate := NewHookGate(svc, hookPath, "")
	gate.Bind(svc)
	t.Cleanup(gate.Close)
	parent := agent.ParentRun{
		RunID: "parent-journal", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Provider: hookAgentTestProvider{}, Model: "fake-model", ProjectRoot: root,
	}
	gate.RunStartRun(context.Background(), parent, session.ID, "start")
	replay, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventHookFired {
			continue
		}
		var fired sessionlog.HookFired
		if err = decodeSessionData(event.Data, &fired); err != nil {
			t.Fatal(err)
		}
		if fired.ChildRunID != "child-journal" || len(fired.Output) > sessionlog.MaxHookOutput || strings.Contains(fired.Output, "token-secret") {
			t.Fatalf("hook journal did not associate and sanitize child result: %+v", fired)
		}
		if strings.Contains(fired.Output, "thinking") || strings.Contains(fired.Output, "transcript") {
			t.Fatalf("hook journal exposed child internal stream: %+v", fired)
		}
		return
	}
	t.Fatal("hook agent result was not journaled")
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
