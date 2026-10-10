package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

// Accepting a Goal-scoped workspace candidate changes project files through
// the review/acceptance path, but it is not itself a Goal observation or proof.
func TestAcceptedGoalWorkspaceCandidateDoesNotCreateGoalEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "result.txt")
	if err := os.WriteFile(formalFile, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "Goal workspace acceptance evidence boundary")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: "goal-workspace-evidence", Objective: "verify accepted implementation independently",
		AllowedRoot: formalAbs, AllowedCapabilities: []string{"kicad.repair_connection"},
		SourceSessionID: session.ID,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	goalWork := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-workspace-evidence", WorkItemID: "item-evidence"}
	scope := workspace.Scope{
		ProjectID: "goalproject", SessionID: session.ID, Work: goalWork,
		Authority: permission.Authority{
			RunID: "workspace-setup-run", SessionID: session.ID, GoalID: goalWork.GoalID,
			WorkItemID: goalWork.WorkItemID, AllowedRoot: formalAbs, FormalRoot: formalAbs,
			CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{
		Exporter: workspaceCandidateExporter{service: service},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	created, err := manager.Create(ctx, scope, "goal implementation")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "result.txt"), []byte("workspace result"), 0600); err != nil {
		t.Fatal(err)
	}
	goalBefore, err := db.GetGoalSnapshot(ctx, goalWork.GoalID)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := manager.Export(ctx, scope, created.ID)
	if err != nil || exported.CandidateID == "" || exported.State != workspace.StateExported {
		t.Fatalf("export Goal workspace candidate=%+v err=%v", exported, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("review exported candidate=%+v err=%v", review, err)
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
		t.Fatalf("accept reviewed Goal candidate: receipt=%+v err=%v", receipt, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "workspace result" {
		t.Fatalf("accepted candidate formal content=%q err=%v", got, err)
	}
	goalAfter, err := db.GetGoalSnapshot(ctx, goalWork.GoalID)
	if err != nil {
		t.Fatal(err)
	}
	if goalAfter.Goal.Status != core.GoalPendingReverification || goalAfter.Goal.Reason != "candidate accepted; independent reverification required" {
		t.Fatalf("accepted workspace candidate did not require independent Goal reverification: before=%+v after=%+v", goalBefore.Goal, goalAfter.Goal)
	}
	if goalAfter.Goal.Status == core.GoalVerified || len(goalAfter.Observations) != 0 || len(goalAfter.Evidence) != 0 {
		t.Fatalf("candidate acceptance became Goal evidence or verification: observations=%+v evidence=%+v goal=%+v", goalAfter.Observations, goalAfter.Evidence, goalAfter.Goal)
	}
	if len(goalAfter.Events) != 1 || string(goalAfter.Events[0].Kind) != "candidate_accepted" {
		t.Fatalf("candidate acceptance Goal event=%+v; want one candidate_accepted notification", goalAfter.Events)
	}
}
