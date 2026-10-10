package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestBoundLeadStaleExecutorCannotWriteAfterNextGenerationStarts(t *testing.T) {
	ctx := context.Background()
	formalRoot := t.TempDir()
	formalFile := filepath.Join(formalRoot, "formal.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(formalRoot, "stale workspace executor")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	svc := &Service{
		deps:       Deps{Store: db, ProjectRoot: formalRoot, WorkspaceStateRoot: stateRoot},
		workspaces: map[string]*workspace.LifecycleService{},
	}
	manager, err := svc.workspaceService(formalRoot)
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
	formal, err := filepath.Abs(formalRoot)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SessionID: session.ID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(formal, "unused-candidate"),
		Mode: permission.ModeBypass,
	}
	created, err := manager.Create(ctx, scope, "lead writer generation check")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &leadRunToolSandbox{}
	baseFactory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: leadRunAllowGate{}, Sandbox: sandbox, HelperPath: helperPath,
	})
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}
	newExecutor := func(runID string) (workspace.WriterLease, agent.RunExecutor) {
		t.Helper()
		runScope := scope
		runScope.OriginRunID = runID
		runScope.Authority.RunID = runID
		lease, acquireErr := manager.AcquireLeadWriter(ctx, runScope, created.ID, runID)
		if acquireErr != nil {
			t.Fatalf("acquire lead writer %s: %v", runID, acquireErr)
		}
		factory := execution.WorkspaceWriterExecutorFactory(baseFactory, lease, manager)
		if factory == nil {
			t.Fatal("trusted workspace writer factory was not derived")
		}
		bounds, marshalErr := json.Marshal(lease.Authority)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		executor, executorErr := factory.ForRun(agent.ExecutionRequest{RunID: runID, Work: work, PermissionBounds: bounds})
		if executorErr != nil {
			t.Fatalf("create trusted executor for %s: %v", runID, executorErr)
		}
		return lease, executor
	}

	oldLease, oldExecutor := newExecutor("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	write := func(executor agent.RunExecutor, id, name, content string) (agent.ToolOutcome, error) {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"file_path": name, "content": content})
		return executor.Execute(ctx, llm.ToolUse{ID: id, Name: "write_file", Arguments: args})
	}
	if outcome, err := write(oldExecutor, "old-write", "old-generation.txt", "old generation"); err != nil || outcome.IsError {
		t.Fatalf("old generation initial write failed: outcome=%+v err=%v", outcome, err)
	}
	if _, err := manager.ReleaseCompletedWriter(ctx, oldLease); err != nil {
		t.Fatalf("settle old generation: %v", err)
	}
	settled, err := manager.Get(ctx, scope, created.ID)
	if err != nil || settled.State != workspace.StateKept || settled.WriterRunID != "" || settled.Generation != oldLease.Generation {
		t.Fatalf("old generation did not settle: workspace=%+v err=%v", settled, err)
	}

	newLease, currentExecutor := newExecutor("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if newLease.Generation <= oldLease.Generation || newLease.RunID == oldLease.RunID {
		t.Fatalf("new lease did not advance generation: old=%+v new=%+v", oldLease, newLease)
	}
	checkoutFile := filepath.Join(newLease.Paths.Checkout, "stale-generation.txt")
	if outcome, err := write(oldExecutor, "stale-write", "stale-generation.txt", "must not appear"); err != nil || !outcome.IsError {
		t.Fatalf("stale executor was not denied: outcome=%+v err=%v", outcome, err)
	}
	if _, err := os.Lstat(checkoutFile); !os.IsNotExist(err) {
		t.Fatalf("stale executor changed the checkout: lstat err=%v", err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal baseline" {
		t.Fatalf("stale executor changed formal project: content=%q err=%v", got, err)
	}
	sandbox.mu.Lock()
	callsAfterStaleAttempt := append([]string(nil), sandbox.calls...)
	sandbox.mu.Unlock()
	if len(callsAfterStaleAttempt) != 1 {
		t.Fatalf("stale write reached the sandbox: calls=%v", callsAfterStaleAttempt)
	}
	if outcome, err := write(currentExecutor, "current-write", "new-generation.txt", "current generation"); err != nil || outcome.IsError {
		t.Fatalf("current generation write failed: outcome=%+v err=%v", outcome, err)
	}
	if got, err := os.ReadFile(filepath.Join(newLease.Paths.Checkout, "new-generation.txt")); err != nil || string(got) != "current generation" {
		t.Fatalf("current generation write missing from checkout: content=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal baseline" {
		t.Fatalf("current generation write escaped into formal project: content=%q err=%v", got, err)
	}
	if _, err := manager.ReleaseCompletedWriter(ctx, newLease); err != nil {
		t.Fatalf("settle current generation: %v", err)
	}
}
