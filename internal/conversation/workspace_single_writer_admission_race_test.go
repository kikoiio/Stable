package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type admissionRunHandle struct {
	events chan agent.ExecutionEvent
	done   chan agent.RunOutcome
	once   sync.Once
}

type singleWorkspaceAdmissionRunner struct {
	started chan agent.ExecutionRequest
	mu      sync.Mutex
	runs    map[string]*admissionRunHandle
}

func newSingleWorkspaceAdmissionRunner() *singleWorkspaceAdmissionRunner {
	return &singleWorkspaceAdmissionRunner{started: make(chan agent.ExecutionRequest, 4), runs: make(map[string]*admissionRunHandle)}
}

func (r *singleWorkspaceAdmissionRunner) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, errors.New("workspace-bound runs must use a trusted executor")
}

func (r *singleWorkspaceAdmissionRunner) StartWithExecutorFactory(_ context.Context, request agent.ExecutionRequest, _ agent.ExecutorFactory) (*agent.RunHandle, error) {
	handle := &admissionRunHandle{events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1)}
	r.mu.Lock()
	if r.runs[request.RunID] != nil {
		r.mu.Unlock()
		return nil, errors.New("duplicate runner start")
	}
	r.runs[request.RunID] = handle
	r.mu.Unlock()
	r.started <- request
	return &agent.RunHandle{Events: handle.events, Done: handle.done}, nil
}

func (r *singleWorkspaceAdmissionRunner) Cancel(runID string) error {
	r.mu.Lock()
	handle := r.runs[runID]
	r.mu.Unlock()
	if handle == nil {
		return errors.New("unknown run")
	}
	handle.once.Do(func() {
		handle.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
		close(handle.done)
		close(handle.events)
	})
	return nil
}

func TestConcurrentLegalRunsAdmitOneWriterPerWorkspaceGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "base.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(".tmp", "m09-writer-admission-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	runner := newSingleWorkspaceAdmissionRunner()
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service, err := Serve(ctx, Deps{
		Store: db, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: sandbox.New(), HelperPath: helperPath}),
		ProjectRoot:     formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	sessions, err := Request(ctx, service.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	_, scope, err := service.workspaceScope(ctx, ClientMsg{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "setup-run", SessionID: sessionID, AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := service.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "single writer race")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatalf("bind workspace: %v", err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "existing.txt")
	if err := os.WriteFile(checkoutFile, []byte("checkout stays intact"), 0600); err != nil {
		t.Fatal(err)
	}
	readySnapshot, err := manager.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialBinding, err := manager.Binding(scope)
	if err != nil || initialBinding != created.ID {
		t.Fatalf("initial owner binding=%q err=%v want=%q", initialBinding, err, created.ID)
	}

	firstID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	makeMessage := func(runID string) ClientMsg {
		return ClientMsg{SessionID: sessionID, Run: &agent.ExecutionRequest{
			RunID: runID, Work: scope.Work, Intent: "concurrently acquire the same bound workspace",
			Model: "fixture",
		}}
	}
	type startResult struct {
		runID string
		err   error
	}
	ready := make(chan struct{}, 2)
	barrier := make(chan struct{})
	results := make(chan startResult, 2)
	for _, runID := range []string{firstID, secondID} {
		runID := runID
		go func() {
			ready <- struct{}{}
			<-barrier
			results <- startResult{runID: runID, err: service.startRun(ctx, makeMessage(runID), make(chan ServerMsg, 16))}
		}()
	}
	<-ready
	<-ready
	close(barrier)
	firstResult, secondResult := <-results, <-results
	accepted, rejected := firstResult, secondResult
	if accepted.err != nil {
		accepted, rejected = secondResult, firstResult
	}
	if accepted.err != nil || !errors.Is(rejected.err, workspace.ErrOwnership) {
		t.Fatalf("concurrent writer admission results: accepted=%+v rejected=%+v; want one lease and one ownership refusal", accepted, rejected)
	}
	select {
	case started := <-runner.started:
		if started.RunID != accepted.runID || started.Work != scope.Work {
			t.Fatalf("runner received wrong winning authority: %+v accepted=%+v", started, accepted)
		}
	case <-ctx.Done():
		t.Fatal("winning writer did not reach runner")
	}
	select {
	case extra := <-runner.started:
		t.Fatalf("rejected writer also reached runner: %+v", extra)
	default:
	}
	active, err := manager.Get(ctx, scope, created.ID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != accepted.runID || active.Generation != readySnapshot.Generation+1 {
		t.Fatalf("same-workspace race did not persist exactly one writer generation: snapshot=%+v err=%v ready=%+v winner=%+v", active, err, readySnapshot, accepted)
	}
	assertStableOwner := func(op string) {
		t.Helper()
		binding, err := manager.Binding(scope)
		if err != nil || binding != initialBinding {
			t.Fatalf("%s changed owner binding: got=%q err=%v want=%q", op, binding, err, initialBinding)
		}
		snapshot, err := manager.Get(ctx, scope, created.ID)
		if err != nil || snapshot.Generation != active.Generation || snapshot.WriterRunID != active.WriterRunID || snapshot.State != active.State {
			t.Fatalf("%s changed owner writer state: got=%+v err=%v want=%+v", op, snapshot, err, active)
		}
		if bytes, err := os.ReadFile(checkoutFile); err != nil || string(bytes) != "checkout stays intact" {
			t.Fatalf("%s changed checkout: bytes=%q err=%v", op, bytes, err)
		}
		if bytes, err := os.ReadFile(formalFile); err != nil || string(bytes) != "formal baseline" {
			t.Fatalf("%s changed formal bytes: bytes=%q err=%v", op, bytes, err)
		}
	}
	assertStableOwner("rejected concurrent run")

	kept, err := manager.Keep(ctx, scope, created.ID)
	if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" || kept.Generation != active.Generation {
		t.Fatalf("settle winning writer before re-admission: snapshot=%+v err=%v active=%+v", kept, err, active)
	}
	if binding, err := manager.Binding(scope); err != nil || binding != initialBinding {
		t.Fatalf("Keep changed bound workspace: binding=%q err=%v", binding, err)
	}
	if err := service.startRun(ctx, makeMessage(rejected.runID), make(chan ServerMsg, 16)); err != nil {
		t.Fatalf("previously rejected run did not acquire after settlement: %v", err)
	}
	select {
	case started := <-runner.started:
		if started.RunID != rejected.runID || started.Work != scope.Work {
			t.Fatalf("re-admitted run has wrong authority: %+v", started)
		}
	case <-ctx.Done():
		t.Fatal("re-admitted writer did not reach runner")
	}
	secondActive, err := manager.Get(ctx, scope, created.ID)
	if err != nil || secondActive.State != workspace.StateWriting || secondActive.WriterRunID != rejected.runID || secondActive.Generation != active.Generation+1 {
		t.Fatalf("writer did not receive next generation after settlement: snapshot=%+v err=%v previous=%+v", secondActive, err, active)
	}
	if _, err := manager.Keep(ctx, scope, created.ID); err != nil {
		t.Fatalf("settle re-admitted writer: %v", err)
	}
}
