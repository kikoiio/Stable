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
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestGoalWorkspaceAgentCannotAcceptOrCommitReviewedCandidate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formalRoot := filepath.Join(root, "goal-project")
	if err := os.Mkdir(formalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formalRoot, "board.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "agent acceptance authority boundary")
	if err != nil {
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
	formalAbs, err := filepath.Abs(formalRoot)
	if err != nil {
		t.Fatal(err)
	}
	const goalID = "goal-agent-acceptance-boundary"
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: goalID, Objective: "make a workspace change for user review", AllowedRoot: formalAbs,
		AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: session.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runner := newLeadWorkspaceRunner(nil)
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		deps: Deps{
			Store: db, Runner: runner,
			ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
				Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helperPath,
			}),
			ProjectRoot: root, WorkspaceStateRoot: filepath.Join(root, "workspace-state"), Model: "fixture",
			CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
		},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{},
		runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{},
		workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	manager, err := service.workspaceService(formalRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-acceptance"}
	_, scope, err := service.workspaceScope(ctx, ClientMsg{
		SessionID: session.ID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "workspace-setup-run", SessionID: session.ID, GoalID: work.GoalID, WorkItemID: work.WorkItemID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs, CandidateRoot: filepath.Join(formalRoot, "unused-candidate"),
	}

	// Create and review a real candidate belonging to this Goal before the
	// model run, so an attempted acceptance has a valid target and digest.
	candidateWorkspace, err := manager.Create(ctx, scope, "candidate for user review")
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "workspace-state", scope.ProjectID, candidateWorkspace.ID, "checkout")
	if err := os.WriteFile(filepath.Join(checkout, "board.txt"), []byte("reviewed workspace change"), 0600); err != nil {
		t.Fatal(err)
	}
	exported, err := manager.Export(ctx, scope, candidateWorkspace.ID)
	if err != nil || exported.CandidateID == "" {
		t.Fatalf("export Goal workspace candidate=%+v err=%v", exported, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" {
		t.Fatalf("review candidate=%+v err=%v", review, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}

	// Use a separate, active Goal worktree for the real lead actor. The
	// acceptance target remains a valid reviewed candidate from the same Goal.
	activeWorkspace, err := manager.Create(ctx, scope, "active Goal writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, activeWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: "goal-agent-acceptance-attempt", Work: work, Intent: "accept or publish the reviewed workspace change",
		Messages: []llm.Message{{Role: "user", Content: "attempt the requested candidate action"}},
	}
	if err := service.startRun(ctx, ClientMsg{SessionID: session.ID, Run: &request}, make(chan ServerMsg, 8)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Goal-bound workspace lead run did not start")
	}
	runner.mu.Lock()
	gotRequest, factory := runner.request, runner.factory
	runner.mu.Unlock()
	if factory == nil || gotRequest.Work != work {
		t.Fatalf("trusted Goal writer factory/request missing: factory=%T work=%+v", factory, gotRequest.Work)
	}
	for _, schema := range gotRequest.ToolSchemas {
		switch schema.Name {
		case "review_accept", "candidate_accept", "worktree_accept", "git_commit", "git_merge", "git_push", "commit", "merge", "push":
			t.Fatalf("Goal writer exposed user-only/publish action schema %q", schema.Name)
		}
	}
	executor, err := factory.ForRun(gotRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		name string
		args string
	}{
		{"review_accept", `{"candidate_id":"` + exported.CandidateID + `","decision_id":"` + decisionID + `","preview_digest":"` + review.Digest + `","candidate_digest":"` + review.CandidateDigest + `","formal_digest":"` + review.FormalDigest + `","mode":"normal"}`},
		{"candidate_accept", `{"candidate_id":"` + exported.CandidateID + `"}`},
		{"worktree_accept", `{"candidate_id":"` + exported.CandidateID + `"}`},
		{"git_commit", `{"candidate_id":"` + exported.CandidateID + `"}`},
		{"git_merge", `{"candidate_id":"` + exported.CandidateID + `"}`},
		{"git_push", `{"candidate_id":"` + exported.CandidateID + `"}`},
	} {
		outcome, execErr := executor.Execute(ctx, llm.ToolUse{
			ID: "forged-" + attempt.name, Name: attempt.name, Arguments: json.RawMessage(attempt.args),
		})
		if execErr != nil || !outcome.IsError || outcome.Status == agent.ToolSucceeded {
			t.Fatalf("forged model action %q was not rejected: outcome=%+v err=%v", attempt.name, outcome, execErr)
		}
	}
	if receipt, ok, err := db.FindAcceptanceReceipt(ctx, decisionID); err != nil || ok {
		t.Fatalf("forged model acceptance produced receipt=%+v ok=%t err=%v", receipt, ok, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal baseline" {
		t.Fatalf("forged model action changed formal bytes=%q err=%v", got, err)
	}
	stored, err := db.GetCandidate(ctx, exported.CandidateID)
	if err != nil || stored.Candidate.Status != "reviewed" || stored.Candidate.CandidateDigest != review.CandidateDigest {
		t.Fatalf("forged model action changed reviewed candidate=%+v err=%v", stored.Candidate, err)
	}
	if _, err := manager.Keep(ctx, scope, activeWorkspace.ID); err != nil {
		t.Fatalf("stop active Goal writer: %v", err)
	}
}
