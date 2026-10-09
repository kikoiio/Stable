package conversation

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func TestAgentTaskEncodedInputLimitExactBoundaryAndOverflowNoOp(t *testing.T) {
	const maxEncodedBytes = 64 << 10
	const generatedIDLength = 32 // sessionlog.NewID returns 16 random bytes as hex.

	role := "---\nname: limit-reviewer\ndescription: encoded input limit fixture\n---\nReview the assigned area."
	makeInstruction := func(t *testing.T, svc *Service, wantBytes int) string {
		t.Helper()
		definition, ok := svc.deps.Agents.Resolve("limit-reviewer")
		if !ok {
			t.Fatal("limit-reviewer definition was not loaded")
		}
		prefix := definition.Instruction + "\n\nAssigned task:\n"
		probe := agent.DelegationTask{ID: strings.Repeat("0", generatedIDLength), Name: definition.Name, Instruction: prefix}
		encodedProbe, err := json.Marshal([]agent.DelegationTask{probe})
		if err != nil {
			t.Fatal(err)
		}
		payloadBytes := wantBytes - len(encodedProbe)
		if payloadBytes < 1 {
			t.Fatalf("invalid fixture sizing: probe=%d target=%d", len(encodedProbe), wantBytes)
		}
		instruction := strings.Repeat("a", payloadBytes)
		probe.Instruction += instruction
		encoded, err := json.Marshal([]agent.DelegationTask{probe})
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) != wantBytes {
			t.Fatalf("fixture serialized to %d bytes, want %d", len(encoded), wantBytes)
		}
		return instruction
	}

	t.Run("exact maximum is accepted", func(t *testing.T) {
		var runnerCalls atomic.Int32
		var encodedSize atomic.Int64
		runner := agentTaskTestRunner(func(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
			runnerCalls.Add(1)
			encoded, err := json.Marshal([]agent.DelegationTask{input.Task})
			if err != nil {
				return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
			}
			encodedSize.Store(int64(len(encoded)))
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "bounded input accepted"}
		})
		svc, _, sessionID := newAgentTaskTestService(t, runner, role)
		parent := agentTaskTestParent(t, svc, sessionID, "encoded-limit-parent-exact")
		instruction := makeInstruction(t, svc, maxEncodedBytes)
		task, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "limit-reviewer", Instruction: instruction})
		if err != nil {
			t.Fatalf("exact %d-byte encoded task was rejected: %v", maxEncodedBytes, err)
		}
		if task.Status != agent.DelegationSucceeded || runnerCalls.Load() != 1 {
			t.Fatalf("exact-boundary task status=%q runner calls=%d", task.Status, runnerCalls.Load())
		}
		if got := encodedSize.Load(); got != maxEncodedBytes {
			t.Fatalf("runner received %d encoded bytes, want exactly %d", got, maxEncodedBytes)
		}
	})

	t.Run("one byte over is rejected without effects", func(t *testing.T) {
		var runnerCalls atomic.Int32
		runner := agentTaskTestRunner(func(context.Context, agent.ChildRunInput) agent.ChildRunResult {
			runnerCalls.Add(1)
			return agent.ChildRunResult{Status: agent.DelegationSucceeded}
		})
		svc, root, sessionID := newAgentTaskTestService(t, runner, role)
		parent := agentTaskTestParent(t, svc, sessionID, "encoded-limit-parent-over")
		instruction := makeInstruction(t, svc, maxEncodedBytes+1)
		before, err := sessionlog.Replay(root, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "limit-reviewer", Instruction: instruction}); err == nil {
			t.Fatalf("%d-byte encoded task was accepted", maxEncodedBytes+1)
		}
		after, err := sessionlog.Replay(root, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Events) != len(before.Events) {
			t.Fatalf("over-limit rejection appended session facts: before=%d after=%d", len(before.Events), len(after.Events))
		}
		if runnerCalls.Load() != 0 {
			t.Fatalf("over-limit rejection invoked child %d times", runnerCalls.Load())
		}
		coordinator := svc.deps.AgentTasks
		coordinator.mu.Lock()
		active := len(coordinator.active)
		coordinator.mu.Unlock()
		if active != 0 {
			t.Fatalf("over-limit rejection left %d active tasks", active)
		}
	})
}
