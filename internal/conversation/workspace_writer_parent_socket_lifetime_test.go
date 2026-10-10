package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

type parentCompletionWriterProvider struct {
	mu            sync.Mutex
	calls         int
	task          agent.AgentTaskSnapshot
	parentWaiting chan<- struct{}
	parentResume  <-chan struct{}
}

func (p *parentCompletionWriterProvider) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	events := make(chan llm.Event, 2)
	errs := make(chan error, 1)
	if call == 1 {
		args, _ := json.Marshal(map[string]any{
			"agent_name": "builder", "instruction": "write a scoped workspace change", "background": true,
		})
		events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: "launch-writer", Name: "run_agent", Arguments: args, Complete: true}}
	} else {
		snapshot, err := toolChainTaskResult(request, "launch-writer")
		if err != nil {
			errs <- err
		} else {
			p.mu.Lock()
			p.task = snapshot
			p.mu.Unlock()
		}
		if p.parentWaiting != nil {
			p.parentWaiting <- struct{}{}
		}
		if p.parentResume != nil {
			select {
			case <-p.parentResume:
			case <-ctx.Done():
				errs <- ctx.Err()
				close(events)
				close(errs)
				return events, errs
			}
		}
		events <- llm.Event{Kind: llm.TextDelta, Text: "Parent run finished while the accepted writer continues."}
	}
	events <- llm.Event{Kind: llm.StreamEnd}
	close(events)
	close(errs)
	return events, errs
}

func TestWorkspaceWriterFinishesAfterParentRunCompletesOverSocket(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		if err := os.WriteFile(filepath.Join(input.ProjectRoot, "writer-output.txt"), []byte("workspace-only"), 0600); err != nil {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
		}
		invocation := agentTaskTestInvocation{ctx: ctx, input: input, release: release}
		select {
		case entered <- invocation:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
		select {
		case <-release:
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "workspace write complete"}
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
	})
	role := "---\nname: builder\ndescription: isolated writer\nisolation: worktree\n---\nWrite only the assigned checkout.\n"
	svc, root, session := newAgentTaskTestService(t, runner, role)
	formal := filepath.Join(root, "formal.txt")
	const formalBytes = "formal baseline"
	if err := os.WriteFile(formal, []byte(formalBytes), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot, err := os.MkdirTemp(filepath.Dir(root), "workspace-parent-socket-state-")
	if err != nil {
		t.Fatal(err)
	}
	svc.deps.WorkspaceStateRoot = stateRoot
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })

	provider := &parentCompletionWriterProvider{}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: agentTaskToolChainGate{}, SessionRoot: root, Provider: provider, ProviderCredential: "fixture-secret",
	}, execution.WithAgentTaskService(svc.deps.AgentTasks), execution.WithDelegator(svc.deps.Delegator, provider))
	schemas := execution.AgentTaskToolSchemas()
	svc.deps.Runner = agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: schemas, MaxRetries: -1})
	svc.deps.ExecutorFactory = factory
	svc.deps.ToolSchemas = schemas
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session},
		Intent: "start a background workspace writer", Messages: []llm.Message{{Role: "user", Content: "write the isolated change"}},
		ProviderName: "fixture", Model: "parent-model",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := OpenRun(ctx, svc.deps.SocketPath, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var outcome *agent.RunOutcome
	for outcome == nil {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if message.Type == "error" {
			t.Fatalf("parent run error: %s", message.Error)
		}
		if message.Type == "run_outcome" && message.RunID == runID {
			outcome = message.Outcome
		}
	}
	if outcome.Status != agent.RunCompleted {
		t.Fatalf("parent outcome=%+v", outcome)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	invocation := receiveAgentTaskInvocation(t, entered)
	if err := invocation.ctx.Err(); err != nil {
		t.Fatalf("client socket close canceled accepted background writer: %v", err)
	}
	provider.mu.Lock()
	task := provider.task
	provider.mu.Unlock()
	if task.ID == "" || task.Status.IsTerminal() || task.OriginRunID != runID {
		t.Fatalf("parent did not persist active writer handle before completion: %+v", task)
	}
	if invocation.input.WorkspaceID != task.WorkspaceID || invocation.input.WorkspaceGeneration != task.WorkspaceGeneration || task.WorkspaceID == "" || task.WorkspaceGeneration == 0 {
		t.Fatalf("writer lost workspace generation: child=%q/%d task=%q/%d", invocation.input.WorkspaceID, invocation.input.WorkspaceGeneration, task.WorkspaceID, task.WorkspaceGeneration)
	}

	projectRoot, scope, err := svc.workspaceScope(context.Background(), ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != task.RunID || active.Generation != task.WorkspaceGeneration {
		t.Fatalf("writer lease after parent terminal/socket close=%+v err=%v task=%+v", active, err, task)
	}
	parent, err := svc.forkParentRun(context.Background(), session, runID)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := svc.deps.AgentTasks.Output(context.Background(), parent, task.ID, 0)
	if err != nil || visible.Status.IsTerminal() || visible.RunID != task.RunID {
		t.Fatalf("background task not queryable while writer is active: %+v err=%v", visible, err)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed before writer completion: %q err=%v", got, err)
	}

	releaseOnce.Do(func() { close(release) })
	terminal := waitAgentTaskTerminal(t, svc, parent, task.ID)
	settled, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || terminal.Status != agent.DelegationSucceeded || terminal.Summary != "workspace write complete" || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != task.WorkspaceGeneration {
		t.Fatalf("writer failed to finish and settle after parent completion: task=%+v workspace=%+v err=%v", terminal, settled, err)
	}
	if got, err := os.ReadFile(filepath.Join(invocation.input.ProjectRoot, "writer-output.txt")); err != nil || string(got) != "workspace-only" {
		t.Fatalf("workspace result not preserved: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed after writer completion: %q err=%v", got, err)
	}
}

func TestWorkspaceWriterSurvivesOpenRunSocketDisconnect(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	releaseWriter := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseWriter) }) })
	runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		if err := os.WriteFile(filepath.Join(input.ProjectRoot, "writer-output.txt"), []byte("workspace-only"), 0600); err != nil {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
		}
		invocation := agentTaskTestInvocation{ctx: ctx, input: input, release: releaseWriter}
		select {
		case entered <- invocation:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
		select {
		case <-releaseWriter:
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "workspace write complete"}
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
	})
	role := "---\nname: builder\ndescription: isolated writer\nisolation: worktree\n---\nWrite only the assigned checkout.\n"
	svc, root, session := newAgentTaskTestService(t, runner, role)
	formal := filepath.Join(root, "formal.txt")
	const formalBytes = "formal baseline"
	if err := os.WriteFile(formal, []byte(formalBytes), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot, err := os.MkdirTemp(filepath.Dir(root), "workspace-disconnect-state-")
	if err != nil {
		t.Fatal(err)
	}
	svc.deps.WorkspaceStateRoot = stateRoot
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })

	parentWaiting := make(chan struct{}, 1)
	resumeParent := make(chan struct{})
	provider := &parentCompletionWriterProvider{parentWaiting: parentWaiting, parentResume: resumeParent}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: agentTaskToolChainGate{}, SessionRoot: root, Provider: provider, ProviderCredential: "fixture-secret",
	}, execution.WithAgentTaskService(svc.deps.AgentTasks), execution.WithDelegator(svc.deps.Delegator, provider))
	schemas := execution.AgentTaskToolSchemas()
	svc.deps.Runner = agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: schemas, MaxRetries: -1})
	svc.deps.ExecutorFactory = factory
	svc.deps.ToolSchemas = schemas
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session},
		Intent: "start background workspace writer", Messages: []llm.Message{{Role: "user", Content: "write isolated change"}},
		ProviderName: "fixture", Model: "parent-model",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := OpenRun(ctx, svc.deps.SocketPath, request)
	if err != nil {
		t.Fatal(err)
	}
	invocation := receiveAgentTaskInvocation(t, entered)
	select {
	case <-parentWaiting:
	case <-ctx.Done():
		t.Fatal("parent did not reach its held post-tool provider call")
	}
	provider.mu.Lock()
	task := provider.task
	provider.mu.Unlock()
	if task.ID == "" || task.Status.IsTerminal() || task.OriginRunID != runID {
		t.Fatalf("background handle before disconnect=%+v", task)
	}
	if invocation.ctx.Err() != nil || invocation.input.WorkspaceID != task.WorkspaceID || invocation.input.WorkspaceGeneration != task.WorkspaceGeneration || task.WorkspaceID == "" || task.WorkspaceGeneration == 0 {
		t.Fatalf("child lease/context before disconnect: ctx=%v input=%q/%d task=%q/%d", invocation.ctx.Err(), invocation.input.WorkspaceID, invocation.input.WorkspaceGeneration, task.WorkspaceID, task.WorkspaceGeneration)
	}

	svc.mu.Lock()
	serviceRunDone := svc.runDone[runID]
	svc.mu.Unlock()
	if serviceRunDone == nil {
		t.Fatal("parent run was not registered as active")
	}
	select {
	case <-serviceRunDone:
		t.Fatal("parent run reached terminal before client disconnect")
	default:
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serviceRunDone:
		t.Fatal("closing the real OpenRun socket unexpectedly completed the held parent run")
	default:
	}
	if err := invocation.ctx.Err(); err != nil {
		t.Fatalf("client disconnect canceled accepted background writer: %v", err)
	}

	projectRoot, scope, err := svc.workspaceScope(context.Background(), ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != task.RunID || active.Generation != task.WorkspaceGeneration {
		t.Fatalf("writer lease after real socket disconnect=%+v err=%v task=%+v", active, err, task)
	}
	parent, err := svc.forkParentRun(context.Background(), session, runID)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := svc.deps.AgentTasks.Output(context.Background(), parent, task.ID, 0)
	if err != nil || visible.Status.IsTerminal() || visible.RunID != task.RunID {
		t.Fatalf("writer task not queryable after disconnect: %+v err=%v", visible, err)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed during held parent: %q err=%v", got, err)
	}

	close(resumeParent)
	select {
	case <-serviceRunDone:
	case <-ctx.Done():
		t.Fatal("parent did not complete after resuming its provider")
	}
	if invocation.ctx.Err() != nil {
		t.Fatalf("parent terminal after disconnect canceled background writer: %v", invocation.ctx.Err())
	}
	releaseOnce.Do(func() { close(releaseWriter) })
	terminal := waitAgentTaskTerminal(t, svc, parent, task.ID)
	settled, err := manager.Get(context.Background(), scope, task.WorkspaceID)
	if err != nil || terminal.Status != agent.DelegationSucceeded || terminal.Summary != "workspace write complete" || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != task.WorkspaceGeneration {
		t.Fatalf("writer did not terminally settle after disconnect: task=%+v workspace=%+v err=%v", terminal, settled, err)
	}
	if got, err := os.ReadFile(filepath.Join(invocation.input.ProjectRoot, "writer-output.txt")); err != nil || string(got) != "workspace-only" {
		t.Fatalf("checkout result lost after disconnect: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(formal); err != nil || string(got) != formalBytes {
		t.Fatalf("formal bytes changed after disconnect settlement: %q err=%v", got, err)
	}
	svc.deps.AgentTasks.mu.Lock()
	_, orphaned := svc.deps.AgentTasks.active[task.ID]
	svc.deps.AgentTasks.mu.Unlock()
	if orphaned {
		t.Fatalf("terminal writer remained in active task map: %s", task.ID)
	}
}
