package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceResolutionExpiresWhenCheckoutChangesBeforeExport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "board.txt")
	if err := os.WriteFile(formalFile, []byte("formal-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace resolution source change")
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
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "resolutionproject")
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
			t.Errorf("close workspace service: %v", err)
		}
	})
	scope := workspace.Scope{
		ProjectID: "resolutionproject", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "setup-run", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "unused-candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "stale workspace resolution")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "board.txt")
	if err := os.WriteFile(checkoutFile, []byte("workspace-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal-v2"), 0600); err != nil {
		t.Fatal(err)
	}
	firstPreview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || firstPreview.ConflictCount != 1 || len(firstPreview.Conflicts) != 1 || firstPreview.Conflicts[0] != "board.txt" {
		t.Fatalf("initial conflict preview=%+v err=%v", firstPreview, err)
	}
	firstResolution, err := manager.ResolveUser(ctx, scope, created.ID, "user", firstPreview.PreviewID, firstPreview.Generation, map[string]string{"board.txt": workspace.UseWorkspace})
	if err != nil || firstResolution.ResolvedCount != 1 {
		t.Fatalf("initial user resolution=%+v err=%v", firstResolution, err)
	}

	// The workspace source changes after the user chose which version to keep.
	if err := os.WriteFile(checkoutFile, []byte("workspace-v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export accepted a resolution after the workspace digest changed")
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal-v2" {
		t.Fatalf("failed stale-resolution export changed formal content: content=%q err=%v", got, err)
	}

	currentPreview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || currentPreview.PreviewID == firstPreview.PreviewID || currentPreview.WorkspaceDigest == firstPreview.WorkspaceDigest || currentPreview.ConflictCount != 1 {
		t.Fatalf("changed checkout did not produce a fresh conflict preview: initial=%+v current=%+v err=%v", firstPreview, currentPreview, err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export reused the prior user resolution without a fresh confirmation")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user", currentPreview.PreviewID, currentPreview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err != nil {
		t.Fatalf("fresh resolution for current workspace digest: %v", err)
	}
	exported, err := manager.Export(ctx, scope, created.ID)
	if err != nil || exported.CandidateID == "" {
		t.Fatalf("freshly resolved workspace export=%+v err=%v", exported, err)
	}
	record, err := db.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(record.Candidate.CandidateRoot, "board.txt")); err != nil || string(got) != "workspace-v2" {
		t.Fatalf("candidate did not contain the freshly confirmed workspace bytes: content=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal-v2" {
		t.Fatalf("export changed formal content before acceptance: content=%q err=%v", got, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("fresh candidate review=%+v err=%v", review, err)
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
		t.Fatalf("fresh candidate acceptance receipt=%+v err=%v", receipt, err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "workspace-v2" {
		t.Fatalf("accepted formal content=%q err=%v; want latest confirmed workspace bytes", got, err)
	}
}
