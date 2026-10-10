package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestBoundLeadAgentCannotSubmitUserConflictResolution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formalFile := filepath.Join(root, "board.txt")
	if err := os.WriteFile(formalFile, []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "agent cannot resolve user conflict")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	runner := newLeadWorkspaceRunner(nil)
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	service := &Service{
		deps: Deps{
			Store: db, Runner: runner,
			ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
				Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helperPath,
			}),
			ProjectRoot: root, WorkspaceStateRoot: stateRoot, Model: "fixture",
		},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{},
		runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{},
		workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := service.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	_, scope, err := service.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "setup-run", SessionID: session.ID, AllowedRoot: formal,
		FormalRoot: formal, CandidateRoot: filepath.Join(root, "unused-candidate"),
	}
	created, err := manager.Create(ctx, scope, "conflicted agent workspace")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(stateRoot, formal, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	if err := layout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte("workspace edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal edit"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("prepare exact user conflict: preview=%+v err=%v", preview, err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 16)
	request := agent.ExecutionRequest{
		RunID: "agent-resolution-attempt", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Intent: "resolve the conflict", Messages: []llm.Message{{Role: "user", Content: "choose the workspace version"}},
	}
	if err := service.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted bound agent runner did not start")
	}
	runner.mu.Lock()
	gotRequest, factory := runner.request, runner.factory
	runner.mu.Unlock()
	if factory == nil {
		t.Fatal("bound lead run did not receive its trusted executor factory")
	}
	for _, schema := range gotRequest.ToolSchemas {
		if schema.Name == "worktree_resolve" {
			t.Fatal("agent tool schema exposed the user-only worktree_resolve operation")
		}
	}
	executor, err := factory.ForRun(gotRequest)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := executor.Execute(ctx, llm.ToolUse{
		ID: "forged-user-resolution", Name: "worktree_resolve",
		Arguments: json.RawMessage(`{"workspace_id":"` + created.ID + `","preview_id":"` + preview.PreviewID + `","generation":1,"choices":{"board.txt":"use_workspace"}}`),
	})
	if err != nil || attempt.Status != agent.ToolDenied || !attempt.IsError {
		t.Fatalf("agent resolution attempt=%+v err=%v; want ToolDenied", attempt, err)
	}
	current, err := manager.Get(ctx, scope, created.ID)
	if err != nil || current.ResolutionID != "" || current.ResolvedCount != 0 || current.CandidateID != "" {
		t.Fatalf("rejected agent attempt changed conflict decision/export state: snapshot=%+v err=%v", current, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal edit" {
		t.Fatalf("rejected agent attempt changed formal bytes: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(paths.Checkout, "board.txt")); err != nil || string(got) != "workspace edit" {
		t.Fatalf("rejected agent attempt changed workspace bytes: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(paths.Baseline, "board.txt")); err != nil || string(got) != "baseline" {
		t.Fatalf("rejected agent attempt changed baseline bytes: %q err=%v", got, err)
	}
	if _, err := manager.Keep(ctx, scope, created.ID); err != nil {
		t.Fatalf("stop bound agent after the denied attempt: %v", err)
	}
}
