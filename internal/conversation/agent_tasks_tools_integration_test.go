package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

type agentTaskToolChainGate struct{}

func (agentTaskToolChainGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "fixture allow"}, nil
}

type agentTaskToolChainProvider struct {
	mu         sync.Mutex
	requests   []llm.Request
	child      <-chan agentTaskTestInvocation
	invocation agentTaskTestInvocation
	task       agent.AgentTaskSnapshot
}

func (p *agentTaskToolChainProvider) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	step := len(p.requests)
	p.mu.Unlock()
	events := make(chan llm.Event, 2)
	errs := make(chan error, 1)
	fail := func(err error) { errs <- err; close(errs); close(events) }
	tool := func(id, name string, args any) {
		encoded, _ := json.Marshal(args)
		events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: id, Name: name, Arguments: encoded, Complete: true}}
	}
	switch step {
	case 1:
		tool("launch-task", "run_agent", map[string]any{"agent_name": "explore", "instruction": "Find the active configuration file.", "background": true})
	case 2:
		snapshot, err := toolChainTaskResult(request, "launch-task")
		if err != nil {
			fail(err)
			return events, errs
		}
		if snapshot.ID == "" || snapshot.RunID == "" || snapshot.Status.IsTerminal() {
			fail(fmt.Errorf("background launch did not return an active handle: %+v", snapshot))
			return events, errs
		}
		var invocation agentTaskTestInvocation
		select {
		case invocation = <-p.child:
		case <-ctx.Done():
			fail(ctx.Err())
			return events, errs
		case <-time.After(3 * time.Second):
			fail(fmt.Errorf("background child never started"))
			return events, errs
		}
		p.mu.Lock()
		p.task = snapshot
		p.invocation = invocation
		p.mu.Unlock()
		close(invocation.release)
		tool("read-task", "task_output", map[string]any{"task_id": snapshot.ID, "block": true, "wait_ms": 3000})
	case 3:
		snapshot, err := toolChainTaskResult(request, "read-task")
		if err != nil {
			fail(err)
			return events, errs
		}
		if snapshot.Status != agent.DelegationSucceeded || snapshot.Summary != "safe findings" {
			fail(fmt.Errorf("parent did not receive the persisted child result: %+v", snapshot))
			return events, errs
		}
		events <- llm.Event{Kind: llm.TextDelta, Text: "Investigation finished using the background task findings."}
	default:
		events <- llm.Event{Kind: llm.TextDelta, Text: "Follow-up completed."}
	}
	events <- llm.Event{Kind: llm.StreamEnd}
	close(events)
	close(errs)
	return events, errs
}
func toolChainTaskResult(request llm.Request, callID string) (agent.AgentTaskSnapshot, error) {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		for _, result := range request.Messages[i].ToolResults {
			if result.ToolUseID != callID {
				continue
			}
			var snapshot agent.AgentTaskSnapshot
			if result.IsError {
				return snapshot, fmt.Errorf("tool %s failed: %s", callID, result.Content)
			}
			if err := json.Unmarshal([]byte(result.Content), &snapshot); err != nil {
				return snapshot, err
			}
			return snapshot, nil
		}
	}
	return agent.AgentTaskSnapshot{}, fmt.Errorf("missing %s tool result", callID)
}
func runAgentTaskToolChainSocket(t *testing.T, svc *Service, request agent.ExecutionRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := OpenRun(ctx, svc.deps.SocketPath, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := stream.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		message, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if message.Type == "error" {
			t.Fatalf("run service error: %s", message.Error)
		}
		if message.Type == "run_outcome" && message.RunID == request.RunID {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				t.Fatalf("parent outcome=%+v", message.Outcome)
			}
			return
		}
	}
}

func TestAgentTaskFullParentToolChainThroughSocketSessionAndGoal(t *testing.T) {
	for _, kind := range []agent.WorkKind{agent.WorkSession, agent.WorkGoal} {
		t.Run(string(kind), func(t *testing.T) {
			entered := make(chan agentTaskTestInvocation, 1)
			svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
			provider := &agentTaskToolChainProvider{child: entered}
			factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: agentTaskToolChainGate{}, SessionRoot: root, Provider: provider, ProviderCredential: "secret-token"}, execution.WithAgentTaskService(svc.deps.AgentTasks), execution.WithDelegator(svc.deps.Delegator, provider))
			schemas := execution.AgentTaskToolSchemas()
			svc.deps.Runner = agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: schemas, MaxRetries: -1})
			svc.deps.ExecutorFactory = factory
			svc.deps.ToolSchemas = schemas
			work := agent.WorkRef{Kind: kind, SessionID: session}
			allowedRoot := root
			if kind == agent.WorkGoal {
				allowedRoot = filepath.Join(root, "board")
				if err := os.Mkdir(allowedRoot, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.deps.Store.CreateGoal(context.Background(), coreGoal("tool-goal", allowedRoot, session)); err != nil {
					t.Fatal(err)
				}
				work.GoalID, work.WorkItemID = "tool-goal", "tool-item"
			}
			request := agent.ExecutionRequest{RunID: "tool-parent", Work: work, Intent: "Investigate the configuration", Messages: []llm.Message{{Role: "user", Content: "PARENT_HISTORY_SENTINEL inspect the configuration"}}, ProviderName: "fixture", Model: "parent-model"}
			runAgentTaskToolChainSocket(t, svc, request)
			provider.mu.Lock()
			task := provider.task
			child := provider.invocation.input
			provider.mu.Unlock()
			if task.ID == "" || child.ParentRunID != task.RunID || child.Work != work || child.ProjectRoot != allowedRoot || child.Model != "parent-model" || strings.Contains(child.Task.Instruction, "PARENT_HISTORY_SENTINEL") {
				t.Fatalf("child context/ownership=%+v task=%+v", child, task)
			}
			if len(child.ToolSchemas) != 3 {
				t.Fatalf("child lost its read-only tools: %+v", child.ToolSchemas)
			}
			for _, schema := range child.ToolSchemas {
				if schema.Name != "read_file" && schema.Name != "glob" && schema.Name != "grep" {
					t.Fatalf("recursive or writable child schema: %s", schema.Name)
				}
			}
			var authority permission.Authority
			if json.Unmarshal(child.PermissionBounds, &authority) != nil || authority.RunID != child.ChildRunID || authority.SessionID != session || authority.GoalID != work.GoalID || authority.WorkItemID != work.WorkItemID || authority.AllowedRoot != allowedRoot {
				t.Fatalf("child authority=%+v", authority)
			}
			// The first later model run consumes the durable summary notification;
			// the next model run consumes none. Neither starts another child.
			for _, runID := range []string{"notification-parent", "after-notification-parent"} {
				next := request
				next.RunID = runID
				next.Messages = []llm.Message{{Role: "user", Content: "Continue analysis"}}
				runAgentTaskToolChainSocket(t, svc, next)
			}
			provider.mu.Lock()
			requests := append([]llm.Request(nil), provider.requests...)
			provider.mu.Unlock()
			if len(requests) != 5 {
				t.Fatalf("unexpected provider calls: %d", len(requests))
			}
			countNotifications := func(request llm.Request) int {
				count := 0
				for _, message := range request.Messages {
					if strings.HasPrefix(message.Content, "Agent task result (reference data):\n") {
						count++
					}
				}
				return count
			}
			if countNotifications(requests[3]) != 1 || countNotifications(requests[4]) != 0 {
				t.Fatalf("notification handoff counts=%d/%d", countNotifications(requests[3]), countNotifications(requests[4]))
			}
			transcript, err := sessionlog.Replay(root, session)
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]sessionlog.ToolCall{}
			results := map[string]sessionlog.ToolResult{}
			taskStarts, parentTerminals, childTerminals, notifications := 0, 0, 0, 0
			for _, event := range transcript.Events {
				switch event.Type {
				case sessionlog.EventToolCall:
					var call sessionlog.ToolCall
					if decodeSessionData(event.Data, &call) == nil {
						calls[call.CallID] = call
					}
				case sessionlog.EventToolResult:
					var result sessionlog.ToolResult
					if decodeSessionData(event.Data, &result) == nil {
						results[result.CallID] = result
					}
				case sessionlog.EventRunStarted:
					var start sessionlog.RunStarted
					if decodeSessionData(event.Data, &start) == nil && start.AgentTaskID != "" {
						taskStarts++
						if start.AgentTaskID != task.ID || start.OriginRunID != request.RunID || start.OriginCallID != "launch-task" {
							t.Fatalf("task origin=%+v", start)
						}
					}
				case sessionlog.EventAgentTaskNotification:
					notifications++
				case sessionlog.EventRunEvent:
					var run sessionlog.RunEvent
					if decodeSessionData(event.Data, &run) == nil && run.Kind == "terminal" {
						if run.RunID == task.RunID {
							childTerminals++
						} else {
							parentTerminals++
						}
					}
				}
			}
			if len(calls) != 2 || len(results) != 2 || calls["launch-task"].Name != "run_agent" || calls["read-task"].Name != "task_output" || calls["launch-task"].RunID != request.RunID || calls["read-task"].RunID != request.RunID || results["launch-task"].Error != "" || results["read-task"].Error != "" || taskStarts != 1 || childTerminals != 1 || parentTerminals != 3 || notifications != 1 {
				t.Fatalf("audit calls=%+v results=%+v starts=%d child-term=%d parent-term=%d notices=%d", calls, results, taskStarts, childTerminals, parentTerminals, notifications)
			}
			select {
			case <-entered:
				t.Fatal("query, notification, or follow-up reran child")
			default:
			}
		})
	}
}
