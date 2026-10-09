package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type agentTaskTestRunner func(context.Context, agent.ChildRunInput) agent.ChildRunResult

func (f agentTaskTestRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	return f(ctx, input)
}

type agentTaskTestInvocation struct {
	ctx     context.Context
	input   agent.ChildRunInput
	release chan struct{}
}

func agentTaskBarrierRunner(entered chan<- agentTaskTestInvocation) agentTaskTestRunner {
	return func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		invocation := agentTaskTestInvocation{ctx: ctx, input: input, release: make(chan struct{})}
		select {
		case entered <- invocation:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled}
		}
		select {
		case <-invocation.release:
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "safe findings"}
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
	}
}
func receiveAgentTaskInvocation(t *testing.T, entered <-chan agentTaskTestInvocation) agentTaskTestInvocation {
	t.Helper()
	select {
	case invocation := <-entered:
		return invocation
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
		return agentTaskTestInvocation{}
	}
}

func newAgentTaskTestService(t *testing.T, runner agent.ChildRunner, definitions string) (*Service, string, string) {
	t.Helper()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := agentcatalog.New("", "")
	if definitions != "" {
		dir := filepath.Join(root, "agents")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "role.md"), []byte(definitions), 0600); err != nil {
			t.Fatal(err)
		}
		catalog = agentcatalog.New(dir, "")
		if rejections := catalog.Snapshot().Rejections; len(rejections) > 0 {
			t.Fatalf("definition rejected: %v", rejections)
		}
	}
	reporter := NewDelegationEventReporter()
	pool, err := agent.NewPoolDelegator(agent.DelegationLimits{Workers: 2, QueueCapacity: 4}, runner, reporter)
	if err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "stable-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(context.Background())
	coordinator := NewAgentTaskCoordinator()
	svc, err := Serve(life, Deps{Store: state, ProjectRoot: root, SocketPath: filepath.Join(socketDir, "c.sock"), PollEvery: time.Hour, Agents: catalog, AgentTasks: coordinator, Delegator: pool, ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{}, ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}, {Name: "grep"}, {Name: "glob"}, {Name: "write_file"}, {Name: "run_agent"}}, ProviderName: "fixture", Model: "parent-model", ProviderCredential: "secret-token"})
	if err != nil {
		cancel()
		pool.Close()
		state.Close()
		os.RemoveAll(socketDir)
		t.Fatal(err)
	}
	reporter.Bind(svc)
	session, err := sessionlog.Create(root, "agents")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		coordinator.Close()
		pool.Close()
		coordinator.mu.Lock()
		var pending []<-chan struct{}
		for _, task := range coordinator.active {
			pending = append(pending, task.done)
		}
		coordinator.mu.Unlock()
		for _, done := range pending {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("agent task did not exit during cleanup")
			}
		}
		cancel()
		svc.Close()
		state.Close()
		os.RemoveAll(socketDir)
	})
	return svc, root, session.ID
}
func agentTaskTestParent(t *testing.T, svc *Service, sessionID, runID string) agent.ParentRun {
	t.Helper()
	parent, err := svc.forkParentRun(context.Background(), sessionID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: "session", Intent: "fixture parent"}); err != nil {
		t.Fatal(err)
	}
	return parent
}
func waitAgentTaskTerminal(t *testing.T, svc *Service, parent agent.ParentRun, id string) agent.AgentTaskSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := svc.deps.AgentTasks.Output(ctx, parent, id, 3*time.Second)
	if err != nil || !snapshot.Status.IsTerminal() {
		t.Fatalf("task did not terminate: %+v, %v", snapshot, err)
	}
	return snapshot
}

func TestAgentTaskSocketDisconnectAndParentCompletionKeepBackgroundAlive(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 2)
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	messages, err := Request(ctx, svc.deps.SocketPath, ClientMsg{Op: "agent_task_start", SessionID: session, AgentName: "explore", Text: "inspect direct"})
	cancel() // Request closes the invoking socket; accepted work owns service lifetime.
	if err != nil {
		t.Fatal(err)
	}
	var direct agent.AgentTaskSnapshot
	for _, message := range messages {
		if message.AgentTask != nil {
			direct = *message.AgentTask
		}
	}
	if direct.ID == "" || direct.OriginRunID != "" {
		t.Fatalf("direct result: %+v", direct)
	}
	directInvocation := receiveAgentTaskInvocation(t, entered)
	if err := directInvocation.ctx.Err(); err != nil {
		t.Fatalf("disconnect canceled background child: %v", err)
	}
	parent := agentTaskTestParent(t, svc, session, "parent-run")
	parentCtx, parentCancel := context.WithCancel(context.Background())
	background, err := svc.deps.AgentTasks.Run(parentCtx, parent, agent.AgentTaskRequest{AgentName: "plan", Instruction: "inspect parent", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	invocation := receiveAgentTaskInvocation(t, entered)
	waitCtx, stopWait := context.WithCancel(context.Background())
	stopWait()
	if _, err := svc.deps.AgentTasks.Output(waitCtx, parent, background.ID, time.Minute); err != context.Canceled {
		t.Fatalf("canceled output wait = %v", err)
	}
	if invocation.ctx.Err() != nil {
		t.Fatal("canceling an output wait canceled its task")
	}
	if _, err := sessionlog.Append(root, session, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: "parent-terminal", RunID: parent.RunID, SessionID: session, RunSeq: 1, At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "completed"}}); err != nil {
		t.Fatal(err)
	}
	parentCancel()
	if err := invocation.ctx.Err(); err != nil {
		t.Fatalf("normal parent context cleanup canceled background child: %v", err)
	}
	close(directInvocation.release)
	close(invocation.release)
	if got := waitAgentTaskTerminal(t, svc, parent, background.ID); got.Status != agent.DelegationSucceeded {
		t.Fatalf("parent background=%+v", got)
	}
	if got := waitAgentTaskTerminal(t, svc, parent, direct.ID); got.Status != agent.DelegationSucceeded {
		t.Fatalf("direct background=%+v", got)
	}
}

func TestNamedWorktreeAgentChildReceivesLeaseAcrossEntryModes(t *testing.T) {
	for _, entry := range []string{"sync", "background", "definition"} {
		t.Run(entry, func(t *testing.T) {
			isolation := "none"
			if entry == "definition" {
				isolation = "worktree"
			}
			role := fmt.Sprintf("---\nname: builder\ndescription: isolated writer\nisolation: %s\n---\nWrite only the assigned checkout.\n", isolation)
			seen := make(chan agent.ChildRunInput, 1)
			runner := agentTaskTestRunner(func(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
				seen <- input
				return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "done"}
			})
			svc, root, session := newAgentTaskTestService(t, runner, role)
			stateRoot, err := os.MkdirTemp(filepath.Dir(root), "named-writer-state-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
			svc.deps.WorkspaceStateRoot = stateRoot

			parentRunID, err := sessionlog.NewID()
			if err != nil {
				t.Fatal(err)
			}
			parent := agentTaskTestParent(t, svc, session, parentRunID)
			parent.ExecutorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{})
			svc.mu.Lock()
			svc.activeRuns[parent.RunID] = session
			svc.activeRequests[parent.RunID] = agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
			svc.mu.Unlock()

			req := agent.AgentTaskRequest{AgentName: "builder", Instruction: "write a bounded change", Isolation: "worktree", Background: entry == "background"}
			if entry == "definition" {
				req.Isolation = "" // Definition isolation must select the same leased path.
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			task, err := svc.deps.AgentTasks.Run(ctx, parent, req)
			if err != nil {
				t.Fatal(err)
			}
			if !task.Status.IsTerminal() {
				task, err = svc.deps.AgentTasks.Output(ctx, parent, task.ID, time.Second)
			}
			if err != nil || task.Status != agent.DelegationSucceeded {
				t.Fatalf("worktree task did not complete: %+v err=%v", task, err)
			}
			input := <-seen
			if input.WorkspaceID == "" || input.WorkspaceID != task.WorkspaceID || input.WorkspaceGeneration == 0 || input.WorkspaceGeneration != task.WorkspaceGeneration {
				t.Fatalf("child lost its trusted workspace generation: input=%q/%d task=%q/%d", input.WorkspaceID, input.WorkspaceGeneration, task.WorkspaceID, task.WorkspaceGeneration)
			}
			if filepath.Clean(input.ProjectRoot) == filepath.Clean(root) {
				t.Fatalf("child received formal root instead of its workspace: %q", input.ProjectRoot)
			}
			svc.mu.Lock()
			delete(svc.activeRuns, parent.RunID)
			delete(svc.activeRequests, parent.RunID)
			svc.mu.Unlock()
		})
	}
}

func TestAgentTaskExplicitParentCancelAndSessionOwnership(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 2)
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	otherSession, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	firstParent := agentTaskTestParent(t, svc, session, "parent-first")
	otherParent := agentTaskTestParent(t, svc, otherSession.ID, "parent-other")
	first, err := svc.deps.AgentTasks.Run(context.Background(), firstParent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "first", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	firstInvocation := receiveAgentTaskInvocation(t, entered)
	other, err := svc.deps.AgentTasks.Run(context.Background(), otherParent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "other", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	otherInvocation := receiveAgentTaskInvocation(t, entered)
	if _, err := svc.deps.AgentTasks.Output(context.Background(), otherParent, first.ID, 0); err == nil {
		t.Fatal("cross-session output accepted")
	}
	if _, err := svc.deps.AgentTasks.Stop(context.Background(), otherParent, first.ID); err == nil {
		t.Fatal("cross-session cancel accepted")
	}
	svc.deps.Runner = &delegationEventPublisher{}
	svc.mu.Lock()
	svc.activeRuns[firstParent.RunID] = session
	svc.mu.Unlock()
	if err := svc.cancelRun(ClientMsg{RunID: firstParent.RunID, SessionID: otherSession.ID}, make(chan ServerMsg, 1)); err == nil {
		t.Fatal("cross-session parent cancel accepted")
	}
	if firstInvocation.ctx.Err() != nil {
		t.Fatal("rejected cancel affected first child")
	}
	if err := svc.cancelRun(ClientMsg{RunID: firstParent.RunID, SessionID: session}, make(chan ServerMsg, 1)); err != nil {
		t.Fatal(err)
	}
	if got := waitAgentTaskTerminal(t, svc, firstParent, first.ID); got.Status != agent.DelegationCanceled {
		t.Fatalf("canceled=%+v", got)
	}
	if otherInvocation.ctx.Err() != nil {
		t.Fatal("parent cancellation affected another session")
	}
	close(otherInvocation.release)
	if got := waitAgentTaskTerminal(t, svc, otherParent, other.ID); got.Status != agent.DelegationSucceeded {
		t.Fatalf("other=%+v", got)
	}
	if got, err := svc.deps.AgentTasks.Stop(context.Background(), firstParent, first.ID); err != nil || got.Status != agent.DelegationCanceled {
		t.Fatalf("idempotent stop=%+v, %v", got, err)
	}
	if _, err := svc.deps.AgentTasks.Run(context.Background(), firstParent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "late", Background: true}); err == nil {
		t.Fatal("canceled parent accepted a new task")
	}
}

func TestAgentTaskRoleBudgetModelToolsAndAudit(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	body := "ROLE BODY MUST STAY PRIVATE"
	definition := "---\nname: review\ndescription: read-only review\nmodel: role-model\ntools: [read_file, write_file, run_agent]\nmaxTurns: 2\nbackground: true\n---\n" + body + "\n"
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), definition)
	parent := agentTaskTestParent(t, svc, session, "parent-role")
	snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "review", Instruction: "explicit assignment", Model: "override-model", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	} // Definition background:true makes this nonblocking.
	invocation := receiveAgentTaskInvocation(t, entered)
	input := invocation.input
	if input.Model != "override-model" || input.Budget.MaxToolRounds != 2 || input.Budget.MaxDuration > time.Second {
		t.Fatalf("model/budget=%s %+v", input.Model, input.Budget)
	}
	if input.ParentRunID != snapshot.RunID || input.ParentRunID == parent.RunID || input.Work != parent.Work || input.ProjectRoot != root {
		t.Fatalf("independent work=%+v", input)
	}
	if !strings.Contains(input.Task.Instruction, body) || !strings.Contains(input.Task.Instruction, "explicit assignment") {
		t.Fatal("role and invocation were not composed")
	}
	if len(input.ToolSchemas) != 1 || input.ToolSchemas[0].Name != "read_file" {
		t.Fatalf("schemas=%+v", input.ToolSchemas)
	}
	var bounds permission.Authority
	if err := json.Unmarshal(input.PermissionBounds, &bounds); err != nil {
		t.Fatal(err)
	}
	if bounds.RunID != input.ChildRunID || bounds.SessionID != session || bounds.AllowedRoot != root {
		t.Fatalf("authority=%+v", bounds)
	}
	executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, PermissionBounds: input.PermissionBounds})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "recursive", Name: "run_agent"})
	if err != nil || outcome.Status != agent.ToolDenied {
		t.Fatalf("recursive executor=%+v, %v", outcome, err)
	}
	encoded, _ := json.Marshal(snapshot)
	inventory, _ := json.Marshal(svc.deps.Agents.Snapshot())
	if strings.Contains(string(encoded)+string(inventory), body) {
		t.Fatal("public task/catalog leaked role body")
	}
	close(invocation.release)
	waitAgentTaskTerminal(t, svc, parent, snapshot.ID)
	path, _ := sessionlog.SessionPath(root, session)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), body) {
		t.Fatal("session log leaked role body")
	}
}

func TestAgentTaskSynchronousWaitAndInputRejection(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	svc, _, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	parent := agentTaskTestParent(t, svc, session, "parent-sync")
	type result struct {
		snapshot agent.AgentTaskSnapshot
		err      error
	}
	results := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		snapshot, err := svc.deps.AgentTasks.Run(ctx, parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "wait for investigation"})
		results <- result{snapshot, err}
	}()
	invocation := receiveAgentTaskInvocation(t, entered)
	if invocation.input.Model != "parent-model" {
		t.Fatalf("built-in role failed to inherit model: %s", invocation.input.Model)
	}
	select {
	case early := <-results:
		t.Fatalf("synchronous call returned early: %+v", early)
	default:
	}
	close(invocation.release)
	select {
	case got := <-results:
		if got.err != nil || got.snapshot.Status != agent.DelegationSucceeded {
			t.Fatalf("sync=%+v", got)
		}
	case <-ctx.Done():
		t.Fatal("synchronous call did not finish")
	}
	for _, request := range []agent.AgentTaskRequest{
		{AgentName: "unknown", Instruction: "inspect", Background: true},
		{AgentName: "explore", Instruction: " ", Background: true},
		{AgentName: "explore", Instruction: strings.Repeat("a", 64<<10), Background: true},
		{AgentName: "explore", Instruction: "inspect", Timeout: -1, Background: true},
	} {
		if _, err := svc.deps.AgentTasks.Run(context.Background(), parent, request); err == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
	select {
	case <-entered:
		t.Fatal("invalid request invoked child")
	default:
	}
}

func appendAgentTaskTestFact(t *testing.T, root, session, run string, seq uint64, kind string, payload any) {
	t.Helper()
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: run, SessionID: session, RunSeq: seq, At: time.Now().UTC(), Kind: kind, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}
func startAgentTaskTestFact(t *testing.T, root, session, run, task string, work agent.WorkRef) {
	t.Helper()
	if _, err := sessionlog.Append(root, session, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: run, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "agent task explore", AgentTaskID: task, AgentName: "explore"}); err != nil {
		t.Fatal(err)
	}
}
func seedAgentTaskTestDelegation(task, status string) agent.DelegationEvent {
	return agent.DelegationEvent{BatchID: "batch-" + task, TaskID: task, TaskName: "explore", Status: agent.DelegationStatus(status), Summary: "public findings", UpdatedAt: time.Now().UTC()}
}

func TestAgentTaskRecoveryAllCrashGapsIsIdempotent(t *testing.T) {
	for _, status := range []string{"before-queued", "queued", "running", "succeeded", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			session, err := sessionlog.Create(root, "recovery")
			if err != nil {
				t.Fatal(err)
			}
			startAgentTaskTestFact(t, root, session.ID, "task-run", "task-id", agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID})
			var seq uint64
			if status != "before-queued" {
				seq++
				appendAgentTaskTestFact(t, root, session.ID, "task-run", seq, "delegation_event", seedAgentTaskTestDelegation("task-id", "queued"))
				if status != "queued" {
					seq++
					appendAgentTaskTestFact(t, root, session.ID, "task-run", seq, "delegation_event", seedAgentTaskTestDelegation("task-id", status))
				}
			}
			if err := recoverAgentTaskRuns(root); err != nil {
				t.Fatal(err)
			}
			first, err := sessionlog.Replay(root, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			records, err := sessionlog.AgentTasks(first)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%+v %v", records, err)
			}
			want := agent.DelegationInterrupted
			outcome := "interrupted"
			switch status {
			case "succeeded":
				want = agent.DelegationSucceeded
				outcome = "completed"
			case "failed":
				want = agent.DelegationFailed
				outcome = "failed"
			case "canceled":
				want = agent.DelegationCanceled
				outcome = "cancelled"
			}
			if records[0].Delegation.Status != string(want) || records[0].RunStatus != outcome {
				t.Fatalf("recovered=%+v want=%s/%s", records[0], want, outcome)
			}
			if err := recoverAgentTaskRuns(root); err != nil {
				t.Fatal(err)
			}
			if err := recoverDelegationRuns(root); err != nil {
				t.Fatal(err)
			}
			second, err := sessionlog.Replay(root, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Events) != len(second.Events) {
				t.Fatalf("second recovery changed log length: %d -> %d", len(first.Events), len(second.Events))
			}
		})
	}
}

func TestAgentTaskRecoveryPairsParentToolCallWithoutRerunning(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "tool crash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "parent-run", WorkKind: "session", Intent: "parent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventToolCall, sessionlog.ToolCall{RunID: "parent-run", CallID: "call-agent", Name: "run_agent", Input: map[string]any{"agent": "explore", "instruction": "inspect", "background": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "task-run", WorkKind: "session", Intent: "agent explore", AgentTaskID: "task-id", AgentName: "explore", OriginRunID: "parent-run"}); err != nil {
		t.Fatal(err)
	}
	appendAgentTaskTestFact(t, root, session.ID, "task-run", 1, "delegation_event", seedAgentTaskTestDelegation("task-id", "queued"))
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	first, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paired, childTerminals, parentTerminals int
	for _, e := range first.Events {
		switch e.Type {
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(e.Data, &result) == nil && result.CallID == "call-agent" {
				paired++
			}
		case sessionlog.EventRunEvent:
			var run sessionlog.RunEvent
			if decodeSessionData(e.Data, &run) == nil && run.Kind == "terminal" {
				if run.RunID == "task-run" {
					childTerminals++
				}
				if run.RunID == "parent-run" {
					parentTerminals++
				}
			}
		}
	}
	if paired != 1 || childTerminals != 1 || parentTerminals != 1 {
		t.Fatalf("recovery paired=%d child=%d parent=%d", paired, childTerminals, parentTerminals)
	}
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != len(first.Events) {
		t.Fatal("recovery duplicated parent tool result or terminal")
	}
}

func TestAgentTaskNotificationsRecoverMissingDestinationAndFilterWork(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "notifications")
	if err != nil {
		t.Fatal(err)
	}
	goalWork := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-1", WorkItemID: "item-1"}
	sessionWork := agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}
	for _, item := range []struct {
		run, task string
		work      agent.WorkRef
	}{{"goal-task", "goal-task-id", goalWork}, {"session-task", "session-task-id", sessionWork}} {
		startAgentTaskTestFact(t, root, session.ID, item.run, item.task, item.work)
		appendAgentTaskTestFact(t, root, session.ID, item.run, 1, "delegation_event", seedAgentTaskTestDelegation(item.task, "succeeded"))
		appendAgentTaskTestFact(t, root, session.ID, item.run, 2, "terminal", map[string]string{"status": "completed"})
	}
	svc := &Service{deps: Deps{ProjectRoot: root}}
	first, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: "missing-destination", Work: goalWork})
	if err != nil || len(first) != 1 || !strings.Contains(first[0].Content, "goal-task-id") || strings.Contains(first[0].Content, "session-task-id") {
		t.Fatalf("goal notification=%+v %v", first, err)
	}
	// Restart after the reference was persisted but before destination run_started.
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	second, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: "real-destination", Work: goalWork})
	if err != nil || len(second) != 1 {
		t.Fatalf("lost destination did not replay: %+v %v", second, err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "real-destination", WorkKind: "goal", GoalID: goalWork.GoalID, WorkItemID: goalWork.WorkItemID, Intent: "next goal run"}); err != nil {
		t.Fatal(err)
	}
	third, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: "later-goal-run", Work: goalWork})
	if err != nil || len(third) != 0 {
		t.Fatalf("delivered result replayed: %+v %v", third, err)
	}
	otherGoal := goalWork
	otherGoal.GoalID = "goal-2"
	other, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: "other-goal", Work: otherGoal})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-goal notification=%+v %v", other, err)
	}
	ordinary, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: "ordinary-run", Work: sessionWork})
	if err != nil || len(ordinary) != 1 || !strings.Contains(ordinary[0].Content, "session-task-id") {
		t.Fatalf("session notification=%+v %v", ordinary, err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range transcript.Events {
		if e.Type == sessionlog.EventRunStarted {
			starts++
		}
	}
	if starts != 3 {
		t.Fatalf("notification automatically launched new run: %d starts", starts)
	}
}

func TestAgentTaskListCursorOrdersUpdatesAndServiceCloseCancels(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: session}
	startAgentTaskTestFact(t, root, session, "first-run", "first-task", work)
	startAgentTaskTestFact(t, root, session, "second-run", "second-task", work)
	appendAgentTaskTestFact(t, root, session, "second-run", 1, "delegation_event", seedAgentTaskTestDelegation("second-task", "succeeded"))
	appendAgentTaskTestFact(t, root, session, "second-run", 2, "terminal", map[string]string{"status": "completed"})
	appendAgentTaskTestFact(t, root, session, "first-run", 1, "delegation_event", seedAgentTaskTestDelegation("first-task", "succeeded"))
	appendAgentTaskTestFact(t, root, session, "first-run", 2, "terminal", map[string]string{"status": "completed"})
	page, err := svc.listAgentTasks(session, 0, 1)
	if err != nil || len(page) != 1 || page[0].ID != "second-task" {
		t.Fatalf("first page=%+v %v", page, err)
	}
	next, err := svc.listAgentTasks(session, page[0].Cursor, 1)
	if err != nil || len(next) != 1 || next[0].ID != "first-task" {
		t.Fatalf("cursor skipped update=%+v %v", next, err)
	}
	parent := agentTaskTestParent(t, svc, session, "close-parent")
	running, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "cancel on shutdown", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	receiveAgentTaskInvocation(t, entered)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if got := waitAgentTaskTerminal(t, svc, parent, running.ID); got.Status != agent.DelegationCanceled {
		t.Fatalf("close task=%+v", got)
	}
	if _, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "after close", Background: true}); err == nil {
		t.Fatal("closing service accepted new task")
	}
}

func TestAgentTaskTerminalRedactsCredentialsAndCapsPublicText(t *testing.T) {
	runner := agentTaskTestRunner(func(context.Context, agent.ChildRunInput) agent.ChildRunResult {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Summary: "secret-token " + strings.Repeat("s", 12<<10), Error: "secret-token " + strings.Repeat("e", 2048)}
	})
	svc, root, session := newAgentTaskTestService(t, runner, "")
	parent := agentTaskTestParent(t, svc, session, "redact-parent")
	snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	terminal := waitAgentTaskTerminal(t, svc, parent, snapshot.ID)
	if terminal.Status != agent.DelegationFailed || len(terminal.Summary) > 8<<10 || len(terminal.Error) > 1024 || strings.Contains(terminal.Summary+terminal.Error, "secret-token") {
		t.Fatalf("public terminal not sanitized: %+v", terminal)
	}
	path, _ := sessionlog.SessionPath(root, session)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") {
		t.Fatal("terminal log leaked provider credential")
	}
}

func TestAgentTaskRecoveryAssociatesEachPendingParentCallExactly(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "call association")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "parent-run", WorkKind: "session", Intent: "parent"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"call-first", "call-second"} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventToolCall, sessionlog.ToolCall{RunID: "parent-run", CallID: call, Name: "run_agent", Input: map[string]any{"agent": "explore", "instruction": call}}); err != nil {
			t.Fatal(err)
		}
	}
	// Start in opposite order: temporal proximity cannot identify the owner.
	for _, item := range []struct{ call, run, task, summary string }{{"call-second", "second-run", "second-task", "second findings"}, {"call-first", "first-run", "first-task", "first findings"}} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: item.run, WorkKind: "session", Intent: "agent explore", AgentTaskID: item.task, AgentName: "explore", OriginRunID: "parent-run", OriginCallID: item.call}); err != nil {
			t.Fatal(err)
		}
		terminal := seedAgentTaskTestDelegation(item.task, "succeeded")
		terminal.Summary = item.summary
		appendAgentTaskTestFact(t, root, session.ID, item.run, 1, "delegation_event", terminal)
	}
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]agent.AgentTaskSnapshot{}
	for _, e := range transcript.Events {
		if e.Type != sessionlog.EventToolResult {
			continue
		}
		var result sessionlog.ToolResult
		if decodeSessionData(e.Data, &result) != nil {
			t.Fatal("invalid tool result")
		}
		content, ok := result.Result.(string)
		if !ok {
			t.Fatalf("result not encoded snapshot: %+v", result)
		}
		var snapshot agent.AgentTaskSnapshot
		if json.Unmarshal([]byte(content), &snapshot) != nil {
			t.Fatalf("invalid task snapshot: %s", content)
		}
		results[result.CallID] = snapshot
	}
	if len(results) != 2 || results["call-first"].ID != "first-task" || results["call-first"].Summary != "first findings" || results["call-second"].ID != "second-task" || results["call-second"].Summary != "second findings" {
		t.Fatalf("wrong call associations: %+v", results)
	}
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != len(transcript.Events) {
		t.Fatal("repeat call recovery duplicated facts")
	}
}

func TestAgentTaskNotificationBatchCapsAndContinuation(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		count, summaryBytes, firstCount int
	}{{"count", 25, 32, 20}, {"bytes", 9, 8 << 10, 7}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			session, err := sessionlog.Create(root, "notification batch")
			if err != nil {
				t.Fatal(err)
			}
			work := agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}
			for i := 0; i < tc.count; i++ {
				run, task := fmt.Sprintf("task-run-%d", i), fmt.Sprintf("task-id-%d", i)
				startAgentTaskTestFact(t, root, session.ID, run, task, work)
				terminal := seedAgentTaskTestDelegation(task, "succeeded")
				terminal.Summary = strings.Repeat("s", tc.summaryBytes)
				appendAgentTaskTestFact(t, root, session.ID, run, 1, "delegation_event", terminal)
				appendAgentTaskTestFact(t, root, session.ID, run, 2, "terminal", map[string]string{"status": "completed"})
			}
			svc := &Service{deps: Deps{ProjectRoot: root}}
			seen := map[string]bool{}
			for batch := 0; batch < 3; batch++ {
				destination := fmt.Sprintf("destination-%d", batch)
				messages, err := svc.agentTaskNotifications(agent.ExecutionRequest{RunID: destination, Work: work})
				if err != nil {
					t.Fatal(err)
				}
				if batch == 0 && len(messages) != tc.firstCount {
					t.Fatalf("first batch has %d, want %d", len(messages), tc.firstCount)
				}
				if len(messages) > 20 {
					t.Fatalf("batch exceeds count cap: %d", len(messages))
				}
				totalBytes := 0
				for _, message := range messages {
					totalBytes += len(message.Content)
					parts := strings.SplitN(message.Content, "\n", 2)
					if len(parts) != 2 {
						t.Fatal("notification lacks JSON snapshot")
					}
					var snapshot agent.AgentTaskSnapshot
					if json.Unmarshal([]byte(parts[1]), &snapshot) != nil {
						t.Fatal("notification snapshot invalid")
					}
					if seen[snapshot.ID] {
						t.Fatalf("duplicate terminal handoff: %s", snapshot.ID)
					}
					seen[snapshot.ID] = true
				}
				if totalBytes > 64<<10 {
					t.Fatalf("notification batch exceeds 64 KiB: %d", totalBytes)
				}
				if len(messages) == 0 {
					break
				}
				if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: destination, WorkKind: "session", Intent: "consume notification batch"}); err != nil {
					t.Fatal(err)
				}
			}
			if len(seen) != tc.count {
				t.Fatalf("continuation lost results: %d/%d", len(seen), tc.count)
			}
		})
	}
}

func TestAgentTaskGoalAuthorityMatchesWorkAndRejectsMismatch(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 1)
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), "")
	goalRoot := filepath.Join(root, "board")
	if err := os.Mkdir(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.deps.Store.CreateGoal(context.Background(), coreGoal("goal-agent", goalRoot, session)); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session, GoalID: "goal-agent", WorkItemID: "item-agent"}
	request := agent.ExecutionRequest{RunID: "goal-parent", Work: work, Intent: "investigate"}
	authority, err := BuildAuthority(context.Background(), svc.deps.Store, root, request, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	parent := agent.ParentRun{RunID: request.RunID, Work: work, ProjectRoot: goalRoot, PermissionBounds: bounds, Provider: svc.deps.ForkProvider, ProviderName: "fixture", Model: "goal-model", ToolSchemas: svc.deps.ForkToolSchemas, ExecutorFactory: svc.deps.ForkExecutorFactory}
	if _, err := sessionlog.Append(root, session, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: parent.RunID, WorkKind: "goal", GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: request.Intent}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agent.ParentRun){func(p *agent.ParentRun) { p.Work.GoalID = "other-goal" }, func(p *agent.ParentRun) { p.Work.WorkItemID = "other-item" }, func(p *agent.ParentRun) { p.Work.SessionID = "other-session" }, func(p *agent.ParentRun) { p.ProjectRoot = root }, func(p *agent.ParentRun) { p.RunID = "other-run" }} {
		bad := parent
		change(&bad)
		if _, err := svc.deps.AgentTasks.Run(context.Background(), bad, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect", Background: true}); err == nil {
			t.Fatalf("mismatched authority accepted: %+v", bad.Work)
		}
	}
	snapshot, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect goal", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	invocation := receiveAgentTaskInvocation(t, entered)
	var childAuthority permission.Authority
	if json.Unmarshal(invocation.input.PermissionBounds, &childAuthority) != nil {
		t.Fatal("invalid child authority")
	}
	if invocation.input.Work != work || invocation.input.ProjectRoot != goalRoot || childAuthority.GoalID != work.GoalID || childAuthority.WorkItemID != work.WorkItemID || childAuthority.AllowedRoot != goalRoot || childAuthority.RunID != invocation.input.ChildRunID || snapshot.OriginRunID != parent.RunID {
		t.Fatalf("goal child attribution=%+v authority=%+v task=%+v", invocation.input.Work, childAuthority, snapshot)
	}
	close(invocation.release)
	waitAgentTaskTerminal(t, svc, parent, snapshot.ID)
}

func TestAgentTaskReloadPreservesAcceptedDefinitionSnapshot(t *testing.T) {
	entered := make(chan agentTaskTestInvocation, 2)
	oldDefinition := "---\nname: review\ndescription: review\nmodel: old-model\ntools: [read_file]\nmaxTurns: 2\nbackground: true\n---\nOLD PRIVATE ROLE\n"
	svc, root, session := newAgentTaskTestService(t, agentTaskBarrierRunner(entered), oldDefinition)
	parent := agentTaskTestParent(t, svc, session, "reload-parent")
	first, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "review", Instruction: "old request"})
	if err != nil {
		t.Fatal(err)
	}
	oldInvocation := receiveAgentTaskInvocation(t, entered)
	newDefinition := "---\nname: review\ndescription: changed\nmodel: new-model\ntools: [grep]\nmaxTurns: 3\nbackground: true\n---\nNEW PRIVATE ROLE\n"
	if err := os.WriteFile(filepath.Join(root, "agents", "role.md"), []byte(newDefinition), 0600); err != nil {
		t.Fatal(err)
	}
	if rejections := svc.deps.Agents.Reload().Rejections; len(rejections) != 0 {
		t.Fatalf("reload rejections: %v", rejections)
	}
	second, err := svc.deps.AgentTasks.Run(context.Background(), parent, agent.AgentTaskRequest{AgentName: "review", Instruction: "new request"})
	if err != nil {
		t.Fatal(err)
	}
	newInvocation := receiveAgentTaskInvocation(t, entered)
	if oldInvocation.input.Model != "old-model" || oldInvocation.input.Budget.MaxToolRounds != 2 || !strings.Contains(oldInvocation.input.Task.Instruction, "OLD PRIVATE ROLE") || strings.Contains(oldInvocation.input.Task.Instruction, "NEW PRIVATE ROLE") || len(oldInvocation.input.ToolSchemas) != 1 || oldInvocation.input.ToolSchemas[0].Name != "read_file" {
		t.Fatalf("accepted definition changed: %+v", oldInvocation.input)
	}
	if newInvocation.input.Model != "new-model" || newInvocation.input.Budget.MaxToolRounds != 3 || !strings.Contains(newInvocation.input.Task.Instruction, "NEW PRIVATE ROLE") || strings.Contains(newInvocation.input.Task.Instruction, "OLD PRIVATE ROLE") || len(newInvocation.input.ToolSchemas) != 1 || newInvocation.input.ToolSchemas[0].Name != "grep" {
		t.Fatalf("new task did not use reload: %+v", newInvocation.input)
	}
	close(oldInvocation.release)
	close(newInvocation.release)
	waitAgentTaskTerminal(t, svc, parent, first.ID)
	waitAgentTaskTerminal(t, svc, parent, second.ID)
}

func TestAgentTaskTerminalPersistenceFailureReturnsErrorAndRecovers(t *testing.T) {
	type savedLog struct {
		path string
		raw  []byte
		err  error
	}
	injected := make(chan savedLog, 1)
	var root, session string
	runner := agentTaskTestRunner(func(context.Context, agent.ChildRunInput) agent.ChildRunResult {
		path, err := sessionlog.SessionPath(root, session)
		var raw []byte
		if err == nil {
			raw, err = os.ReadFile(path)
		}
		if err == nil {
			var file *os.File
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
			if err == nil {
				_, err = file.WriteString(`{"unfinished_terminal":`)
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
			}
		}
		injected <- savedLog{path: path, raw: raw, err: err}
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "this result cannot be persisted"}
	})
	var svc *Service
	svc, root, session = newAgentTaskTestService(t, runner, "")
	parent := agentTaskTestParent(t, svc, session, "persist-parent")
	type runResult struct {
		snapshot agent.AgentTaskSnapshot
		err      error
	}
	results := make(chan runResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		snapshot, err := svc.deps.AgentTasks.Run(ctx, parent, agent.AgentTaskRequest{AgentName: "explore", Instruction: "inspect"})
		results <- runResult{snapshot, err}
	}()
	var saved savedLog
	select {
	case saved = <-injected:
	case <-ctx.Done():
		t.Fatal("runner did not inject persistence failure")
	}
	if saved.err != nil {
		t.Fatalf("fixture injection failed: %v", saved.err)
	}
	restoreNeeded := true
	t.Cleanup(func() {
		if restoreNeeded {
			if err := os.WriteFile(saved.path, saved.raw, 0600); err != nil {
				t.Errorf("restore temporary log: %v", err)
			}
		}
	})
	select {
	case result := <-results:
		if result.err == nil || !strings.Contains(result.err.Error(), "persist") || result.snapshot.Status == agent.DelegationSucceeded {
			t.Fatalf("persistence failure reported success: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("synchronous task hung after terminal persistence failure")
	}
	if err := os.WriteFile(saved.path, saved.raw, 0600); err != nil {
		t.Fatal(err)
	}
	restoreNeeded = false
	transcript, err := sessionlog.Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sessionlog.AgentTasks(transcript)
	if err != nil || len(records) != 1 {
		t.Fatalf("restored records=%+v %v", records, err)
	}
	if records[0].Delegation.Status != "running" || records[0].RunStatus != "" || records[0].TerminalSeq != 0 {
		t.Fatalf("failed persistence invented a terminal fact: %+v", records[0])
	}
	if err := recoverAgentTaskRuns(root); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.getAgentTask(session, records[0].Started.AgentTaskID)
	if err != nil || recovered.Status != agent.DelegationInterrupted {
		t.Fatalf("nonterminal task did not recover: %+v %v", recovered, err)
	}
}
