package conversation

import (
	"context"
	"errors"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

type rejectingAgentTaskReporter struct{}

func (rejectingAgentTaskReporter) Publish(_ string, event agent.DelegationEvent) error {
	if event.Status == agent.DelegationQueued {
		return errors.New("injected queued persistence failure")
	}
	return nil
}
func TestAgentTaskQueuedPersistenceFailureHasNoAcceptedReceiptOrChild(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), agentTaskBarrierRunner(entered), rejectingAgentTaskReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	svc.deps.Delegator = pool
	parent := agentTaskTestParent(t, svc, session, "parent-persist")
	snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect", Background: true})
	if err == nil || snapshot.ID != "" {
		t.Fatalf("rejection reported acceptance: %+v %v", snapshot, err)
	}
	select {
	case <-entered:
		t.Fatal("rejected task invoked child")
	default:
	}
	transcript, err := sessionlog.Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil || len(records) != 1 || records[0].RunStatus != "failed" || records[0].Delegation.Status != "" {
		t.Fatalf("failure facts: %+v %v", records, err)
	}
	queried, queryErr := svc.getAgentTask(session, records[0].Started.AgentTaskID)
	if queryErr != nil || queried.Error == "" {
		t.Fatalf("persisted failure lost reason: %+v %v", queried, queryErr)
	}
}
func TestAgentTaskFullQueueRejectsAtServiceWithoutProviderCall(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 2)
	svc, _, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	parent := agentTaskTestParent(t, svc, session, "parent-full")
	for i := 0; i < 6; i++ {
		snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect", Background: true})
		if err != nil || snapshot.ID == "" {
			t.Fatalf("accepted %d: %+v %v", i, snapshot, err)
		}
		if i < 2 {
			receiveAgentTaskInvocation(t, entered)
		}
	}
	snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "overflow", Background: true})
	if !errors.Is(err, agent.ErrDelegationQueueFull) || snapshot.ID != "" {
		t.Fatalf("queue rejection: %+v %v", snapshot, err)
	}
	select {
	case <-entered:
		t.Fatal("overflow called provider")
	case <-time.After(10 * time.Millisecond):
	}
	svc.deps.AgentTasks.Close()
}
func TestAgentCatalogSocketInventoryReloadIsPrivateAndSessionOwned(t *testing.T) {
	svc, _, session := newAgentTaskTestService(t, agentTaskTestRunner(func(context.Context, agent.ChildRunInput) agent.ChildRunResult {
		t.Fatal("catalog called provider")
		return agent.ChildRunResult{}
	}), "")
	for _, op := range []string{"agent_list", "agent_reload"} {
		messages, err := Request(context.Background(), svc.deps.SocketPath, ClientMsg{Op: op, SessionID: session})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || messages[0].Agents == nil || len(messages[0].Agents.Definitions) != 3 {
			t.Fatalf("%s inventory: %+v", op, messages)
		}
		for _, role := range messages[0].Agents.Definitions {
			if !role.ReadOnly || len(role.Tools) != 3 {
				t.Fatalf("role metadata: %+v", role)
			}
		}
	}
	if _, err := Request(context.Background(), svc.deps.SocketPath, ClientMsg{Op: "agent_reload", SessionID: "nonexistent"}); err == nil {
		t.Fatal("unknown session reloaded catalog")
	}
	for _, msg := range []ClientMsg{{Op: "agent_task_start", SessionID: session, AgentName: "explore", Text: "inspect", ProjectRoot: "/tmp"}, {Op: "agent_task_get", SessionID: session, TaskID: "id", WaitMS: 30001}, {Op: "agent_task_list", SessionID: session, Limit: 101}} {
		if validateClient(msg) == nil {
			t.Fatalf("unsafe client input accepted: %+v", msg)
		}
	}
}
