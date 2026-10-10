package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type leadWorkspaceRunner struct {
	mu       sync.Mutex
	request  agent.ExecutionRequest
	factory  agent.ExecutorFactory
	startErr error
	started  chan struct{}
	events   chan agent.ExecutionEvent
	done     chan agent.RunOutcome
	once     sync.Once
}

func newLeadWorkspaceRunner(startErr error) *leadWorkspaceRunner {
	return &leadWorkspaceRunner{startErr: startErr, started: make(chan struct{}), events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1)}
}

func (r *leadWorkspaceRunner) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, errors.New("workspace run must use trusted per-run executor API")
}

func (r *leadWorkspaceRunner) StartWithExecutorFactory(_ context.Context, request agent.ExecutionRequest, factory agent.ExecutorFactory) (*agent.RunHandle, error) {
	r.mu.Lock()
	r.request, r.factory = request, factory
	r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	close(r.started)
	return &agent.RunHandle{Events: r.events, Done: r.done}, nil
}

type leadRunAllowGate struct{}

func (leadRunAllowGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

type leadRunToolSandbox struct {
	mu      sync.Mutex
	calls   []string
	lastErr error
}

func (leadRunToolSandbox) Probe(context.Context, sandbox.SandboxProfile) error { return nil }
func (s *leadRunToolSandbox) RunIsolated(_ context.Context, profile sandbox.SandboxProfile, _ []string, input io.Reader) (sandbox.SandboxResult, error) {
	var request struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}
	if err := json.NewDecoder(input).Decode(&request); err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return sandbox.SandboxResult{}, err
	}
	s.mu.Lock()
	s.calls = append(s.calls, request.Tool)
	s.mu.Unlock()
	response := execution.HelperResponse{Output: "read"}
	if request.Tool == "write_file" {
		name, _ := request.Args["file_path"].(string)
		name = strings.TrimPrefix(name, "/workspace/candidate/")
		content, _ := request.Args["content"].(string)
		if err := os.WriteFile(filepath.Join(profile.CandidateRoot, name), []byte(content), 0600); err != nil {
			s.mu.Lock()
			s.lastErr = err
			s.mu.Unlock()
			return sandbox.SandboxResult{}, err
		}
		response.Output = "written"
		response.Removals = 1
	}
	data, err := json.Marshal(response)
	return sandbox.SandboxResult{Stdout: data}, err
}
func (leadRunToolSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("unused")
}
func (leadRunToolSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("unused")
}
func (leadRunToolSandbox) StopIsolatedSession(context.Context, string) error { return nil }

func (r *leadWorkspaceRunner) Cancel(runID string) error {
	r.once.Do(func() {
		close(r.events)
		r.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
		close(r.done)
	})
	return nil
}

func TestBoundSessionRunUsesTrustedWorkspaceLeaseAndExitWaitsForRunner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace lead")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := newLeadWorkspaceRunner(nil)
	workspaceStateRoot := filepath.Join(t.TempDir(), "workspace-state")
	toolSandbox := &leadRunToolSandbox{}
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		deps: Deps{Store: db, Runner: runner, ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: toolSandbox, HelperPath: helperPath}),
			ProjectRoot: root, WorkspaceStateRoot: workspaceStateRoot, Model: "fixture"},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}, runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{}, workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	created, err := manager.Create(ctx, scope, "bound session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 16)
	svc.clients[updates] = &clientSubscription{ch: updates}
	request := agent.ExecutionRequest{RunID: "bound-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "edit workspace", Messages: []llm.Message{{Role: "user", Content: "edit it"}}}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted runner did not start")
	}
	runner.mu.Lock()
	gotRequest, gotFactory := runner.request, runner.factory
	runner.mu.Unlock()
	if gotFactory == nil || gotRequest.ToolSchemas == nil || len(gotRequest.ToolSchemas) != 6 {
		t.Fatalf("workspace run lacks constrained trusted executor surface: factory=%T schemas=%+v", gotFactory, gotRequest.ToolSchemas)
	}
	writerExecutor, err := gotFactory.ForRun(gotRequest)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := writerExecutor.Execute(ctx, llm.ToolUse{ID: "read-base", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"base.txt"}`)}); err != nil || outcome.IsError {
		t.Fatalf("workspace read tool failed: outcome=%+v err=%v", outcome, err)
	}
	if outcome, err := writerExecutor.Execute(ctx, llm.ToolUse{ID: "write-base", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"base.txt","content":"workspace"}`)}); err != nil || outcome.IsError {
		toolSandbox.mu.Lock()
		calls, sandboxErr := append([]string(nil), toolSandbox.calls...), toolSandbox.lastErr
		toolSandbox.mu.Unlock()
		t.Fatalf("workspace write tool failed: outcome=%+v err=%v sandbox calls=%v last error=%v", outcome, err, calls, sandboxErr)
	}
	var authority permission.Authority
	if err := json.Unmarshal(gotRequest.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	workspaceBytes, err := os.ReadFile(filepath.Join(authority.CandidateRoot, "base.txt"))
	if err != nil || string(workspaceBytes) != "workspace" {
		t.Fatalf("write tool did not update bound checkout: content=%q err=%v", workspaceBytes, err)
	}
	formalBytes, err := os.ReadFile(filepath.Join(root, "base.txt"))
	if err != nil || string(formalBytes) != "formal" {
		t.Fatalf("bound write escaped into formal project: content=%q err=%v", formalBytes, err)
	}
	active, err := manager.Get(ctx, scope, created.ID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != "bound-run" {
		t.Fatalf("active workspace=%+v err=%v", active, err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundStart := false
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunStarted {
			continue
		}
		var started sessionlog.RunStarted
		if err := decodeSessionData(event.Data, &started); err == nil && started.RunID == request.RunID {
			foundStart = started.WorkspaceID == created.ID && started.WorkspaceGeneration == active.Generation
		}
	}
	if !foundStart {
		t.Fatal("RunStarted did not persist the active workspace ID and generation")
	}
	if filepath.Clean(authority.AllowedRoot) == formal || !isWithin(workspaceStateRoot, authority.AllowedRoot) || filepath.Clean(authority.FormalRoot) != formal {
		t.Fatalf("workspace run authority=%+v; must read the bound baseline while retaining formal root", authority)
	}
	if _, err := manager.Keep(ctx, scope, created.ID); err != nil {
		t.Fatalf("keep should cancel and await the bound runner: %v", err)
	}
	keptAfterKeep, err := manager.Get(ctx, scope, created.ID)
	if err != nil || keptAfterKeep.State != workspace.StateKept || keptAfterKeep.WriterRunID != "" || keptAfterKeep.ChangedFiles != 1 {
		t.Fatalf("writer was not kept after Keep: workspace=%+v err=%v", keptAfterKeep, err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("Keep unexpectedly cleared binding: bound=%q err=%v", bound, err)
	}

	// A subsequent ordinary run receives a fresh generation. Exit cancels it
	// and only clears the binding after the runner has fully returned.
	runner2 := newLeadWorkspaceRunner(nil)
	svc.deps.Runner = runner2
	request2 := agent.ExecutionRequest{RunID: "bound-exit-run", Work: request.Work, Intent: "exit workspace", Messages: []llm.Message{{Role: "user", Content: "finish"}}}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request2}, updates); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner2.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second trusted runner did not start")
	}
	if _, err := manager.Exit(ctx, scope); err != nil {
		t.Fatalf("exit should cancel and await the bound runner: %v", err)
	}
	kept, err := manager.Get(ctx, scope, created.ID)
	if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 1 {
		t.Fatalf("writer was not kept after runner exit: workspace=%+v err=%v", kept, err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != "" {
		t.Fatalf("binding after exit=%q err=%v", bound, err)
	}
	for len(updates) > 0 {
		message := <-updates
		if message.Type == "error" && message.Error == "could not finalize candidate: changed candidate is not registered" {
			t.Fatal("workspace writer went through ordinary candidate finalization")
		}
	}
}

func TestBoundSessionRunStartFailureReleasesLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace start failure")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := newLeadWorkspaceRunner(errors.New("runner failed to start"))
	svc := &Service{
		deps: Deps{Store: db, Runner: runner, ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{}),
			ProjectRoot: root, WorkspaceStateRoot: filepath.Join(t.TempDir(), "workspace-state"), Model: "fixture"},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}, runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{}, workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, _ := filepath.Abs(root)
	scope.Authority = permission.Authority{RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	created, err := manager.Create(ctx, scope, "bound session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "failed-bound-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "edit workspace", Messages: []llm.Message{{Role: "user", Content: "edit it"}}}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, make(chan ServerMsg, 16)); err == nil {
		t.Fatal("expected runner start failure")
	}
	settled, err := manager.Get(ctx, scope, created.ID)
	if err != nil || settled.State != workspace.StateKept || settled.WriterRunID != "" {
		t.Fatalf("failed start leaked workspace writer lease: workspace=%+v err=%v", settled, err)
	}
}

func TestBoundSessionWriterRecoveryRetainsLeadRunGeneration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace recovery")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	svc := &Service{deps: Deps{Store: db, ProjectRoot: root, WorkspaceStateRoot: stateRoot}, workspaces: map[string]*workspace.LifecycleService{}}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, _ := filepath.Abs(root)
	runID := "recovery-lead-run"
	scope.OriginRunID = runID
	scope.Authority = permission.Authority{RunID: runID, SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	created, err := manager.Create(ctx, scope, "recover bound session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireLeadWriter(ctx, scope, created.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: string(agent.WorkSession), Intent: "recover writer", WorkspaceID: lease.WorkspaceID, WorkspaceGeneration: lease.Generation}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	svc.workspaceMu.Lock()
	delete(svc.workspaces, root)
	svc.workspaceMu.Unlock()
	recovered, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close(context.Background()) })
	restored, err := recovered.Get(ctx, scope, created.ID)
	if err != nil || restored.State != workspace.StateInterrupted || restored.WriterRunID != runID || restored.Generation != lease.Generation {
		t.Fatalf("recovered lead writer=%+v err=%v lease=%+v", restored, err, lease)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunStarted {
			continue
		}
		var started sessionlog.RunStarted
		if decodeSessionData(event.Data, &started) == nil && started.RunID == runID {
			found = started.WorkspaceID == restored.ID && started.WorkspaceGeneration == restored.Generation
		}
	}
	if !found {
		t.Fatal("recovered RunStarted/workspace generation link was lost")
	}
}

func TestWorkspaceEnterSerializesWithBoundRunAdmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace admission race")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := newLeadWorkspaceRunner(nil)
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		deps: Deps{Store: db, Runner: runner, ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helperPath}),
			ProjectRoot: root, WorkspaceStateRoot: filepath.Join(t.TempDir(), "workspace-state"), Model: "fixture"},
		activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}, runDone: map[string]chan struct{}{}, workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, _ := filepath.Abs(root)
	scope.Authority = permission.Authority{RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	first, err := manager.Create(ctx, scope, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, scope, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, first.ID); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "admission-race-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "race", Messages: []llm.Message{{Role: "user", Content: "start"}}}
	svc.workspaceAdmissionMu.Lock()
	startResult := make(chan error, 1)
	enterResult := make(chan error, 1)
	go func() {
		startResult <- svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, make(chan ServerMsg, 8))
	}()
	go func() { _, enterErr := manager.Enter(ctx, scope, second.ID); enterResult <- enterErr }()
	time.Sleep(10 * time.Millisecond)
	svc.workspaceAdmissionMu.Unlock()
	if err := <-startResult; err != nil {
		t.Fatal(err)
	}
	enterErr := <-enterResult
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	runner.mu.Lock()
	var authority permission.Authority
	if err := json.Unmarshal(runner.request.PermissionBounds, &authority); err != nil {
		runner.mu.Unlock()
		t.Fatal(err)
	}
	runner.mu.Unlock()
	bound, err := manager.Binding(scope)
	if err != nil {
		t.Fatal(err)
	}
	var startedWorkspace string
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if decodeSessionData(event.Data, &started) == nil && started.RunID == request.RunID {
				startedWorkspace = started.WorkspaceID
			}
		}
	}
	if enterErr == nil {
		if bound != second.ID || startedWorkspace != second.ID {
			t.Fatalf("enter won admission but run used a different binding: bound=%s started=%s authority=%+v", bound, startedWorkspace, authority)
		}
	} else if !errors.Is(enterErr, workspace.ErrUnavailable) && !errors.Is(enterErr, workspace.ErrOwnership) || bound != first.ID || startedWorkspace != first.ID {
		t.Fatalf("run admission won but enter changed binding: enter=%v bound=%s started=%s", enterErr, bound, startedWorkspace)
	}
	if err := runner.Cancel(request.RunID); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	runDone := svc.runDone[request.RunID]
	svc.mu.Unlock()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run consumer did not settle after cancellation")
	}
	if _, err := manager.Keep(ctx, scope, bound); err != nil {
		t.Fatalf("keep should serialize with session admission: %v", err)
	}
	if stillBound, err := manager.Binding(scope); err != nil || stillBound != bound {
		t.Fatalf("keep unexpectedly changed binding: bound=%q err=%v", stillBound, err)
	}
	if _, err := manager.Exit(ctx, scope); err != nil {
		t.Fatalf("exit after settled keep: %v", err)
	}
}

func TestServiceCloseCancelsLeadWriterBeforeClosingWorkspaceManager(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace close")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runner := newLeadWorkspaceRunner(nil)
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		deps: Deps{Store: db, Runner: runner, ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helperPath}),
			ProjectRoot: root, WorkspaceStateRoot: stateRoot, Model: "fixture"},
		ln: listener, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}, runDone: map[string]chan struct{}{}, workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, _ := filepath.Abs(root)
	scope.Authority = permission.Authority{RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	created, err := manager.Create(ctx, scope, "close with writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "close-lead-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "close", Messages: []llm.Message{{Role: "user", Content: "work"}}}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, make(chan ServerMsg, 8)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted runner did not start")
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close(context.Background()) })
	settled, err := recovered.Get(ctx, scope, created.ID)
	if err != nil || settled.State != workspace.StateKept || settled.WriterRunID != "" {
		t.Fatalf("service close left a live workspace writer: workspace=%+v err=%v", settled, err)
	}
	if bound, err := recovered.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("service close should retain the session binding: bound=%q err=%v", bound, err)
	}
}
