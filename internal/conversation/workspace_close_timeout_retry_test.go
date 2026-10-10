package conversation

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

// closeTimeoutRunner deliberately ignores Cancel until the test releases it.
// This models a runner that has received cancellation but has not actually
// exited yet, which is the critical Service.Close timeout case.
type closeTimeoutRunner struct {
	started chan struct{}
	events  chan agent.ExecutionEvent
	done    chan agent.RunOutcome
	cancel  chan string
	once    sync.Once
}

func newCloseTimeoutRunner() *closeTimeoutRunner {
	return &closeTimeoutRunner{
		started: make(chan struct{}), events: make(chan agent.ExecutionEvent),
		done: make(chan agent.RunOutcome, 1), cancel: make(chan string, 1),
	}
}

func (r *closeTimeoutRunner) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, errors.New("workspace run must use trusted per-run executor API")
}

func (r *closeTimeoutRunner) StartWithExecutorFactory(_ context.Context, _ agent.ExecutionRequest, _ agent.ExecutorFactory) (*agent.RunHandle, error) {
	close(r.started)
	return &agent.RunHandle{Events: r.events, Done: r.done}, nil
}

func (r *closeTimeoutRunner) Cancel(runID string) error {
	select {
	case r.cancel <- runID:
	default:
	}
	return nil
}

func (r *closeTimeoutRunner) finish(runID string) {
	r.once.Do(func() {
		close(r.events)
		r.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
		close(r.done)
	})
}

func TestServiceCloseTimeoutRetainsWriterForRetryAndRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "close timeout")
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
	runner := newCloseTimeoutRunner()
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	svc := &Service{
		deps: Deps{Store: db, Runner: runner, ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{}),
			ProjectRoot: root, WorkspaceStateRoot: stateRoot, Model: "fixture"},
		lifeCtx: ctx, ln: listener, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{},
		runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{},
		workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate")}
	created, err := manager.Create(ctx, scope, "close timeout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "stuck-close-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "hold writer", Messages: []llm.Message{{Role: "user", Content: "hold"}}}
	if err := svc.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, make(chan ServerMsg, 8)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted runner did not start")
	}
	runDone := svc.runDone[request.RunID]
	t.Cleanup(func() {
		runner.finish(request.RunID)
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Errorf("bound run failed to settle during cleanup")
		}
		_ = manager.Close(context.Background())
	})

	closeErr := svc.Close()
	if closeErr == nil || !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close should report active-run timeout, got %v", closeErr)
	}
	select {
	case canceled := <-runner.cancel:
		if canceled != request.RunID {
			t.Fatalf("Close canceled run %q, want %q", canceled, request.RunID)
		}
	default:
		t.Fatal("Close did not request runner cancellation")
	}
	// A timed-out Close must leave the very same manager and its durable writer
	// lease available. The runner has not emitted Done, so settling or closing
	// this manager here would race a still-authorized checkout writer.
	rootKey, _ := filepath.Abs(root)
	svc.workspaceMu.Lock()
	retained := svc.workspaces[rootKey]
	svc.workspaceMu.Unlock()
	if retained != manager {
		t.Fatalf("Close timeout discarded workspace manager: got %p want %p", retained, manager)
	}
	active, err := manager.Get(ctx, scope, created.ID)
	if err != nil || active.State != workspace.StateWriting || active.WriterRunID != request.RunID {
		t.Fatalf("Close timeout changed live writer lease: snapshot=%+v err=%v", active, err)
	}
	select {
	case <-runDone:
		t.Fatal("Close timeout returned after run was unexpectedly marked done")
	default:
	}

	runner.finish(request.RunID)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not settle after runner actually exited")
	}
	settled, err := manager.Get(ctx, scope, created.ID)
	if err != nil || settled.State != workspace.StateKept || settled.WriterRunID != "" {
		t.Fatalf("runner exit did not settle its writer generation: snapshot=%+v err=%v", settled, err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("timeout/retry path changed workspace binding: bound=%q err=%v", bound, err)
	}

	if err := svc.Close(); err != nil {
		t.Fatalf("second Close should finish after runner exit: %v", err)
	}
	svc.workspaceMu.Lock()
	remaining := len(svc.workspaces)
	svc.workspaceMu.Unlock()
	if remaining != 0 {
		t.Fatalf("second Close retained %d workspace managers", remaining)
	}

	// Reopening the durable service after the successful retry must preserve
	// the kept generation and binding; close timeout must not create an
	// interrupted or stale writer record.
	recovered, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close(context.Background()) })
	recoveredSnapshot, err := recovered.Get(ctx, scope, created.ID)
	if err != nil || recoveredSnapshot.State != workspace.StateKept || recoveredSnapshot.WriterRunID != "" || recoveredSnapshot.Generation != settled.Generation {
		t.Fatalf("post-retry recovery changed settled workspace: snapshot=%+v err=%v; settled=%+v", recoveredSnapshot, err, settled)
	}
	if bound, err := recovered.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("post-retry recovery lost session binding: bound=%q err=%v", bound, err)
	}
}
