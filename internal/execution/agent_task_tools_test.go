package execution

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

type agentTaskServiceStub struct {
	parent   agent.ParentRun
	request  agent.AgentTaskRequest
	taskID   string
	wait     time.Duration
	result   agent.AgentTaskSnapshot
	err      error
	calls    int
	sequence *[]string
}

func (s *agentTaskServiceStub) record(parent agent.ParentRun) {
	s.calls++
	s.parent = parent
	if s.sequence != nil {
		*s.sequence = append(*s.sequence, "service")
	}
}
func (s *agentTaskServiceStub) Run(_ context.Context, parent agent.ParentRun, request agent.AgentTaskRequest) (agent.AgentTaskSnapshot, error) {
	s.record(parent)
	s.request = request
	return s.result, s.err
}
func (s *agentTaskServiceStub) Output(_ context.Context, parent agent.ParentRun, id string, wait time.Duration) (agent.AgentTaskSnapshot, error) {
	s.record(parent)
	s.taskID, s.wait = id, wait
	return s.result, s.err
}
func (s *agentTaskServiceStub) Stop(_ context.Context, parent agent.ParentRun, id string) (agent.AgentTaskSnapshot, error) {
	s.record(parent)
	s.taskID = id
	return s.result, s.err
}

func namedTaskExecutor(t *testing.T, service agent.AgentTaskService, gate PermissionGate, kind agent.WorkKind) agent.RunExecutor {
	t.Helper()
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	if kind == agent.WorkGoal {
		authority.GoalID = "goal-1"
		authority.WorkItemID = "item-2"
	}
	request := m06Request(t, authority)
	request.Work.Kind, request.Work.GoalID, request.Work.WorkItemID = kind, authority.GoalID, authority.WorkItemID
	request.ProviderName = "fake-provider"
	request.RunDeadline = time.Now().Add(time.Minute)
	executor, err := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Provider: testDelegationProvider{}}, WithAgentTaskService(service)).ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestNamedAgentTaskDispatchPreservesTrustedGoalScope(t *testing.T) {
	service := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{ID: "task-1", RunID: "child-1", Status: agent.DelegationQueued}}
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	executor := namedTaskExecutor(t, service, gate, agent.WorkGoal)
	outcome, err := executor.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"Explore","instruction":"find config","background":true,"model":"test-model","timeout_ms":5000}`))
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
		t.Fatalf("dispatch: %+v %v", outcome, err)
	}
	if service.calls != 1 || service.request.AgentName != "explore" || !service.request.Background || service.request.Instruction != "find config" || service.request.Model != "test-model" || service.request.Timeout != 5*time.Second {
		t.Fatalf("request: %+v", service.request)
	}
	if service.parent.Work.Kind != agent.WorkGoal || service.parent.Work.GoalID != "goal-1" || service.parent.Work.WorkItemID != "item-2" || service.parent.ProviderName != "fake-provider" || service.parent.ExecutorFactory == nil || service.parent.Deadline.IsZero() {
		t.Fatalf("lost trusted parent: %+v", service.parent)
	}
	if service.parent.ToolCallID != "call-run_agent" {
		t.Fatalf("trusted call association missing: %q", service.parent.ToolCallID)
	}
	var bounds permission.Authority
	if err := json.Unmarshal(service.parent.PermissionBounds, &bounds); err != nil {
		t.Fatal(err)
	}
	if bounds.GoalID != "goal-1" || bounds.WorkItemID != "item-2" || bounds.RunID != service.parent.RunID || bounds.AllowedRoot != service.parent.ProjectRoot {
		t.Fatalf("lost authority: %+v", bounds)
	}
	if gate.calls != 1 || gate.seen.Kind != permission.OpRead || gate.seen.Name != "run_agent" || gate.seen.Target != bounds.AllowedRoot {
		t.Fatalf("wrong permission operation: %+v", gate.seen)
	}
	childRequest := m06Request(t, bounds)
	childRequest.Work = service.parent.Work
	child, err := service.parent.ExecutorFactory.ForRun(childRequest)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := child.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"explore","instruction":"recurse"}`))
	if err != nil || !blocked.IsError || service.calls != 1 {
		t.Fatal("Goal child inherited recursive named-agent capability")
	}
}

func TestNamedAgentTaskIsolationParsingIsBounded(t *testing.T) {
	request, err := parseAgentTaskRequest(map[string]any{"agent_name": "builder", "instruction": "edit one file", "isolation": "worktree"})
	if err != nil || request.Isolation != "worktree" {
		t.Fatalf("worktree request: %+v %v", request, err)
	}
	if _, err := parseAgentTaskRequest(map[string]any{"agent_name": "builder", "instruction": "edit one file", "isolation": "command"}); err == nil {
		t.Fatal("unsupported isolation mode accepted")
	}
}

func TestNamedTaskOutputAndStopStatuses(t *testing.T) {
	service := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{ID: "task-1", Status: agent.DelegationSucceeded, Summary: "found"}}
	executor := namedTaskExecutor(t, service, policyGate{policy: permission.Policy{}}, agent.WorkSession)
	outcome, err := executor.Execute(context.Background(), m06Call("task_output", `{"task_id":"task-1","block":true,"wait_ms":2000}`))
	if err != nil || outcome.IsError || service.taskID != "task-1" || service.wait != 2*time.Second || !strings.Contains(outcome.Content, "found") {
		t.Fatalf("output: %+v %v", outcome, err)
	}
	if _, err = executor.Execute(context.Background(), m06Call("task_output", `{"task_id":"task-1"}`)); err != nil || service.wait != 0 {
		t.Fatalf("nonblocking wait=%s err=%v", service.wait, err)
	}
	if _, err = executor.Execute(context.Background(), m06Call("task_output", `{"task_id":"task-1","block":true}`)); err != nil || service.wait != 30*time.Second {
		t.Fatalf("default wait=%s err=%v", service.wait, err)
	}
	for _, status := range []agent.DelegationStatus{agent.DelegationQueued, agent.DelegationRunning, agent.DelegationSucceeded, agent.DelegationFailed, agent.DelegationCanceled, agent.DelegationInterrupted} {
		service.result.Status = status
		outcome, err = executor.Execute(context.Background(), m06Call("task_stop", `{"task_id":"task-1"}`))
		wantError := status == agent.DelegationFailed || status == agent.DelegationCanceled || status == agent.DelegationInterrupted
		if err != nil || outcome.IsError != wantError {
			t.Fatalf("status %s: %+v %v", status, outcome, err)
		}
	}
	service.err = errors.New("service failure")
	if outcome, err = executor.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"explore","instruction":"find"}`)); err != nil || !outcome.IsError || !strings.Contains(outcome.Content, "service failure") {
		t.Fatalf("error: %+v %v", outcome, err)
	}
}

func TestNamedTaskArgumentsRejectBeforeGateOrService(t *testing.T) {
	service := &agentTaskServiceStub{}
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	executor := namedTaskExecutor(t, service, gate, agent.WorkSession)
	for _, tc := range []struct{ name, args string }{
		{"run_agent", `{"agent_name":"explore","instruction":""}`},
		{"run_agent", `{"agent_name":"../role","instruction":"find"}`},
		{"run_agent", `{"agent_name":"explore","instruction":true}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","background":"true"}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","timeout_ms":0}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","timeout_ms":180001}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","timeout_ms":1.5}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","model":"provider model"}`},
		{"run_agent", `{"agent_name":"explore","instruction":"find","project_root":"/"}`},
		{"task_output", `{"task_id":"task-1","block":"true"}`},
		{"task_output", `{"task_id":"task-1","wait_ms":1}`},
		{"task_output", `{"task_id":"task-1","block":true,"wait_ms":30001}`},
		{"task_output", `{"task_id":"task-1","block":true,"wait_ms":-1}`},
		{"task_stop", `{"task_id":null}`},
		{"task_stop", `{"task_id":"other","session_id":"other"}`},
	} {
		outcome, err := executor.Execute(context.Background(), m06Call(tc.name, tc.args))
		if err != nil || !outcome.IsError {
			t.Fatalf("accepted invalid %s %s: %+v %v", tc.name, tc.args, outcome, err)
		}
	}
	if gate.calls != 0 || service.calls != 0 {
		t.Fatalf("invalid inputs reached gate/service: %d/%d", gate.calls, service.calls)
	}
}

func TestNamedTaskPermissionAndChildRestrictions(t *testing.T) {
	service := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{Status: agent.DelegationQueued}}
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "fixture denied"}}
	executor := namedTaskExecutor(t, service, gate, agent.WorkSession)
	for _, tc := range []struct{ name, args string }{
		{"run_agent", `{"agent_name":"explore","instruction":"find"}`},
		{"task_output", `{"task_id":"task-1"}`},
		{"task_stop", `{"task_id":"task-1"}`},
	} {
		outcome, err := executor.Execute(context.Background(), m06Call(tc.name, tc.args))
		if err != nil || outcome.Status != agent.ToolDenied || !outcome.IsError {
			t.Fatalf("denied tool reached service: %+v %v", outcome, err)
		}
	}
	if service.calls != 0 {
		t.Fatal("permission denial reached service")
	}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	base := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate}, WithAgentTaskService(service)).(ToolExecutorFactory)
	childFactory := ReadOnlyExecutorFactory(base).(ToolExecutorFactory)
	if childFactory.deps.AgentTasks != nil {
		t.Fatal("child inherited named task service")
	}
	child, err := childFactory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range AgentTaskToolSchemas() {
		outcome, err := child.Execute(context.Background(), llm.ToolUse{ID: "child-call", Name: schema.Name, Arguments: json.RawMessage(`{}`)})
		if err != nil || !outcome.IsError {
			t.Fatalf("recursive child tool accepted: %+v %v", outcome, err)
		}
		for _, readonly := range ReadOnlyToolSchemas() {
			if readonly.Name == schema.Name {
				t.Fatal("named-task schema leaked into child")
			}
		}
		if schema.InputSchema["additionalProperties"] != false {
			t.Fatal("schema allows untrusted extra arguments")
		}
	}
}

func TestNamedTaskHooksAndAuditPairing(t *testing.T) {
	sessionRoot := t.TempDir()
	info, err := sessionlog.Create(sessionRoot, "named tasks")
	if err != nil {
		t.Fatal(err)
	}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	authority.SessionID = info.ID
	sequence := []string{}
	service := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{ID: "task-1", Status: agent.DelegationQueued}, sequence: &sequence}
	hooks := &executorTestHookRunner{sequence: &sequence}
	deps := ToolExecutorDeps{Gate: policyGate{policy: permission.Policy{}}, SessionRoot: sessionRoot, HookRunner: hooks, Provider: testDelegationProvider{}}
	executor, err := NewToolExecutorFactory(deps, WithAgentTaskService(service)).ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executor.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"explore","instruction":"find"}`)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequence, []string{"pre", "service", "post"}) {
		t.Fatalf("hook order=%v", sequence)
	}
	transcript, err := sessionlog.Replay(sessionRoot, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var callCount, resultCount int
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventToolCall:
			callCount++
		case sessionlog.EventToolResult:
			resultCount++
		}
	}
	if callCount != 1 || resultCount != 1 {
		t.Fatalf("unpaired tool audit %d/%d", callCount, resultCount)
	}
	hooks.preRejected = true
	if _, err = executor.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"explore","instruction":"find"}`)); err != nil {
		t.Fatal(err)
	}
	if service.calls != 1 || hooks.postCalls != 1 {
		t.Fatal("pre-hook rejection reached service or post-hook")
	}
}

func TestRunAgentPassesWriterSchemaCeilingOnlyToCoordinator(t *testing.T) {
	service := &agentTaskServiceStub{result: agent.AgentTaskSnapshot{ID: "task-1", Status: agent.DelegationQueued}}
	gate := policyGate{policy: permission.Policy{}}
	executor := namedTaskExecutor(t, service, gate, agent.WorkSession)

	if _, err := executor.Execute(context.Background(), m06Call("run_agent", `{"agent_name":"builder","instruction":"write","isolation":"worktree"}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := schemaNames(service.parent.ToolSchemas), schemaNames(WorkspaceWriterToolSchemas()); !reflect.DeepEqual(got, want) {
		t.Fatalf("run_agent parent schema ceiling=%v, want workspace writer schemas %v", got, want)
	}

	if _, err := executor.Execute(context.Background(), m06Call("task_output", `{"task_id":"task-1"}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := schemaNames(service.parent.ToolSchemas), schemaNames(ReadOnlyToolSchemas()); !reflect.DeepEqual(got, want) {
		t.Fatalf("task_output parent schemas=%v, want read-only schemas %v", got, want)
	}
}

func schemaNames(schemas []llm.ToolSchema) []string {
	names := make([]string, len(schemas))
	for i, schema := range schemas {
		names[i] = schema.Name
	}
	return names
}
