//go:build linux

package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

type namedWriterPermissionGate struct{ allow bool }

func (g *namedWriterPermissionGate) Authorize(_ context.Context, _ permission.Authority, _ permission.Operation) (permission.PermissionDecision, error) {
	if !g.allow {
		return permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "denied by acceptance fixture"}, nil
	}
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

// TestNamedRunAgentWriterFactoryEnforcesPermissionRoleAndLease exercises the
// host run_agent permission gate, then the coordinator-created leased child
// factory and its role allowlist against the real cloud bwrap helper.
func TestNamedRunAgentWriterFactoryEnforcesPermissionRoleAndLease(t *testing.T) {
	helper := os.Getenv("STABLE_M09_WRITER_HELPER")
	volume := os.Getenv("STABLE_M09_VOLUME")
	if helper == "" || volume == "" {
		t.Skip("requires cloud helper and disposable bounded disk volume")
	}
	if err := sandbox.BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}

	role := "---\nname: builder\ndescription: bounded writer\nisolation: worktree\ntools: [write_file]\n---\nWrite only the assigned checkout.\n"
	started := make(chan agent.ChildRunInput, 1)
	runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		started <- input
		executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, PermissionBounds: input.PermissionBounds})
		if err != nil {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
		}
		// Force a tool absent from both the role and its advertised schema. If
		// dispatch reaches bwrap, this marker appears in the leased checkout.
		marker := "must-not-run.txt"
		probe := llm.ToolUse{ID: "forbidden-command", Name: "command", Arguments: json.RawMessage(fmt.Sprintf(`{"command":"printf reached > %s"}`, marker))}
		denied, err := executor.Execute(ctx, probe)
		if err != nil || denied.Status != agent.ToolDenied || !denied.IsError {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("forbidden role tool outcome=%+v err=%v", denied, err)}
		}
		if _, err := os.Stat(filepath.Join(input.ProjectRoot, marker)); !os.IsNotExist(err) {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("forbidden command reached checkout: %v", err)}
		}
		write := llm.ToolUse{ID: "allowed-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"named.txt","content":"leased child bytes"}`)}
		outcome, err := executor.Execute(ctx, write)
		if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("authorized write outcome=%+v err=%v", outcome, err)}
		}
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "bounded named writer complete"}
	})
	stateRoot, err := os.MkdirTemp(volume, "named-agent-writer-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(stateRoot); err != nil {
			t.Errorf("remove settled workspace-state fixture: %v", err)
		}
	})
	svc, formal, session := newAgentTaskTestService(t, runner, role)
	svc.deps.WorkspaceStateRoot = stateRoot
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parent := agentTaskTestParent(t, svc, session, parentRunID)
	svc.mu.Lock()
	svc.activeRuns[parent.RunID] = session
	svc.activeRequests[parent.RunID] = agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
	svc.mu.Unlock()
	t.Cleanup(func() {
		svc.mu.Lock()
		delete(svc.activeRuns, parent.RunID)
		delete(svc.activeRequests, parent.RunID)
		svc.mu.Unlock()
	})

	gate := &namedWriterPermissionGate{}
	baseFactory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Sandbox: sandbox.New(), Gate: gate, HelperPath: helper,
		Provider: forkSkillFixtureProvider{},
	}, execution.WithAgentTaskService(svc.deps.AgentTasks))
	parent.ExecutorFactory = baseFactory
	parent.ToolSchemas = []llm.ToolSchema{{Name: "run_agent"}}
	parentRequest := agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
	parentExecutor, err := baseFactory.ForRun(parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolUse{ID: "denied-parent-dispatch", Name: "run_agent", Arguments: json.RawMessage(`{"agent_name":"builder","instruction":"write the assigned file","isolation":"worktree"}`)}
	denied, err := parentExecutor.Execute(context.Background(), call)
	if err != nil || denied.Status != agent.ToolDenied || !denied.IsError {
		t.Fatalf("denied parent permission outcome=%+v err=%v", denied, err)
	}
	if _, err := receiveNamedWriterStart(t, started); err == nil {
		t.Fatal("denied parent permission dispatched a named writer")
	}
	if got, err := os.ReadFile(filepath.Join(formal, "base.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("denied parent permission changed formal file: %q err=%v", got, err)
	}
	for _, name := range []string{"named.txt", "must-not-run.txt"} {
		if _, err := os.Stat(filepath.Join(formal, name)); !os.IsNotExist(err) {
			t.Fatalf("denied parent permission created formal path %q: %v", name, err)
		}
	}
	stateEntries, err := os.ReadDir(stateRoot)
	if err != nil || len(stateEntries) != 0 {
		t.Fatalf("denied parent permission created workspace state: entries=%v err=%v", stateEntries, err)
	}

	// Retry the real host run_agent dispatch with the parent permission gate
	// allowing it. The coordinator adds the lease and role gate for the child.
	gate.allow = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	authorizedCall := llm.ToolUse{ID: "allowed-parent-dispatch", Name: "run_agent", Arguments: json.RawMessage(`{"agent_name":"builder","instruction":"write the assigned file","isolation":"worktree"}`)}
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, session, sessionlog.EventToolCall, sessionlog.ToolCall{RunID: parent.RunID, CallID: authorizedCall.ID, Name: authorizedCall.Name, Input: map[string]any{"agent_name": "builder", "instruction": "write the assigned file", "isolation": "worktree"}}); err != nil {
		t.Fatal(err)
	}
	dispatched, err := parentExecutor.Execute(ctx, authorizedCall)
	if err != nil || dispatched.Status != agent.ToolSucceeded || dispatched.IsError {
		t.Fatalf("authorized parent dispatch outcome=%+v err=%v", dispatched, err)
	}
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, session, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: authorizedCall.ID, Result: dispatched.Content}); err != nil {
		t.Fatal(err)
	}
	var task agent.AgentTaskSnapshot
	if err := json.Unmarshal([]byte(dispatched.Content), &task); err != nil {
		t.Fatalf("decode named task response: %v; response=%q", err, dispatched.Content)
	}
	input, err := receiveNamedWriterStart(t, started)
	if err != nil {
		t.Fatal(err)
	}
	if input.WorkspaceID == "" || input.WorkspaceGeneration == 0 {
		t.Fatalf("named run_agent child did not receive lease identity: workspace=%q generation=%d", input.WorkspaceID, input.WorkspaceGeneration)
	}
	if len(input.ToolSchemas) != 1 || input.ToolSchemas[0].Name != "write_file" {
		t.Fatalf("named role schema was not constrained: %+v", input.ToolSchemas)
	}
	var childAuthority permission.Authority
	if err := json.Unmarshal(input.PermissionBounds, &childAuthority); err != nil {
		t.Fatal(err)
	}
	if childAuthority.RunID != input.ChildRunID || childAuthority.AllowedRoot != input.ProjectRoot {
		t.Fatalf("child authority is not bound to its leased checkout: %+v input=%+v", childAuthority, input)
	}
	if !task.Status.IsTerminal() {
		task, err = svc.deps.AgentTasks.Output(ctx, parent, task.ID, 30*time.Second)
	}
	if err != nil || task.Status != agent.DelegationSucceeded || task.WorkspaceID != input.WorkspaceID || task.WorkspaceGeneration != input.WorkspaceGeneration {
		t.Fatalf("named writer did not settle on the same lease: task=%+v err=%v", task, err)
	}
	if got, err := os.ReadFile(filepath.Join(input.ProjectRoot, "named.txt")); err != nil || string(got) != "leased child bytes" {
		t.Fatalf("authorized write missing from leased checkout: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "base.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("authorized child changed formal baseline: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(formal, "named.txt")); !os.IsNotExist(err) {
		t.Fatalf("authorized child wrote outside its checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(input.ProjectRoot, "must-not-run.txt")); !os.IsNotExist(err) {
		t.Fatalf("forbidden role tool reached the sandbox: %v", err)
	}

	root, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := manager.Get(ctx, scope, task.WorkspaceID)
	if err != nil || kept.State != workspace.StateKept || kept.Generation != task.WorkspaceGeneration || kept.WriterRunID != "" {
		t.Fatalf("named writer lease was not settled: %+v err=%v", kept, err)
	}
}

func receiveNamedWriterStart(t *testing.T, started <-chan agent.ChildRunInput) (agent.ChildRunInput, error) {
	t.Helper()
	select {
	case input := <-started:
		return input, nil
	case <-time.After(50 * time.Millisecond):
		return agent.ChildRunInput{}, fmt.Errorf("named writer did not start")
	}
}
