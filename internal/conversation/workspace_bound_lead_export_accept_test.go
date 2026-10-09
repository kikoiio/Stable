package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestBoundLeadRunExportReviewAcceptChangesFormalOnlyAfterAcceptance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "board.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "bound lead export acceptance")
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
	service := &Service{
		deps: Deps{
			Store: db, Runner: runner,
			ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helperPath}),
			ProjectRoot:     root, WorkspaceStateRoot: filepath.Join(t.TempDir(), "workspace-state"), Model: "fixture",
			CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
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
	created, err := manager.Create(ctx, scope, "bound lead edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 16)
	request := agent.ExecutionRequest{
		RunID: "bound-export-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Intent: "update board", Messages: []llm.Message{{Role: "user", Content: "update the board"}},
	}
	if err := service.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("trusted bound runner did not start")
	}
	runner.mu.Lock()
	gotRequest, factory := runner.request, runner.factory
	runner.mu.Unlock()
	if factory == nil {
		t.Fatal("bound lead run did not receive its trusted executor factory")
	}
	executor, err := factory.ForRun(gotRequest)
	if err != nil {
		t.Fatal(err)
	}
	readResult, err := executor.Execute(ctx, llm.ToolUse{
		ID: "read-board", Name: "read_file",
		Arguments: json.RawMessage(`{"file_path":"board.txt"}`),
	})
	if err != nil || readResult.IsError {
		t.Fatalf("bound read_file result=%+v err=%v", readResult, err)
	}
	result, err := executor.Execute(ctx, llm.ToolUse{
		ID: "write-board", Name: "write_file",
		Arguments: json.RawMessage(`{"file_path":"board.txt","content":"workspace result"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("bound write_file result=%+v err=%v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "board.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("formal project changed before candidate acceptance: content=%q err=%v", got, err)
	}
	if _, err := manager.Keep(ctx, scope, created.ID); err != nil {
		t.Fatalf("keep and stop bound writer: %v", err)
	}
	kept, err := manager.Get(ctx, scope, created.ID)
	if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" {
		t.Fatalf("workspace was not settled after runner stop: snapshot=%+v err=%v", kept, err)
	}

	exported, err := manager.Export(ctx, scope, created.ID)
	if err != nil || exported.State != workspace.StateExported || exported.CandidateID == "" {
		t.Fatalf("workspace export=%+v err=%v", exported, err)
	}
	record, err := db.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(record.Candidate.CandidateRoot, "board.txt")); err != nil || string(got) != "workspace result" {
		t.Fatalf("exported candidate content=%q err=%v; want bound-run write", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "board.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("export changed formal project before acceptance: content=%q err=%v", got, err)
	}

	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("exported candidate review=%+v err=%v", review, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: exported.CandidateID, SessionID: session.ID, DecisionID: decisionID,
		PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest,
		FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || receipt.ID == "" {
		t.Fatalf("candidate acceptance receipt=%+v err=%v", receipt, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "board.txt")); err != nil || string(got) != "workspace result" {
		t.Fatalf("accepted formal content=%q err=%v; want accepted bound-run output", got, err)
	}
}
