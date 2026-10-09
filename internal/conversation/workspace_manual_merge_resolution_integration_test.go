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

func TestWorkspaceManualThirdValueMergeResolvesAndExportsForReview(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "board.txt")
	if err := os.WriteFile(formalFile, []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace-manual-merge")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service := &Service{deps: Deps{
		Store: state, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formal, "project1")
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
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "manual-merge-origin", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "manual merge")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const formalSide = "formal side"
	const mergedValue = "baseline + formal side + workspace side"
	if err := os.WriteFile(formalFile, []byte(formalSide), 0600); err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "board.txt")
	if err := os.WriteFile(checkoutFile, []byte(mergedValue), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("manual merge conflict preview=%+v err=%v", preview, err)
	}
	if preview.PreviewID == "" || preview.Generation != created.Generation || preview.BaselineDigest == "" || preview.FormalDigest == "" || preview.WorkspaceDigest == "" {
		t.Fatalf("preview lacks current workspace/generation/digest binding: %+v", preview)
	}
	if mergedValue == "baseline" || mergedValue == formalSide {
		t.Fatal("fixture must represent a third value distinct from baseline and formal")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation+1, map[string]string{"board.txt": workspace.UseWorkspace}); err == nil {
		t.Fatal("resolution accepted a stale workspace generation")
	}
	otherSession := scope
	otherSession.SessionID = "0123456789abcdef0123456789abcdef"
	otherSession.Work = agent.WorkRef{Kind: agent.WorkSession, SessionID: otherSession.SessionID}
	if _, err := manager.ResolveUser(ctx, otherSession, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err == nil {
		t.Fatal("resolution accepted a different session binding")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user-a", "0123456789abcdef0123456789abcdef", preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err == nil {
		t.Fatal("resolution accepted a different preview binding")
	}
	resolution, err := manager.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace})
	if err != nil || resolution.ResolvedCount != 1 || resolution.ResolutionID == "" {
		t.Fatalf("manual merged value user resolution=%+v err=%v", resolution, err)
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user-b", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseFormal}); err == nil {
		t.Fatal("a second user replaced the resolution bound to the first user")
	}
	if err := os.WriteFile(checkoutFile, []byte(mergedValue+" edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export reused a resolution after the checkout digest changed")
	}
	stalePreview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || stalePreview.PreviewID == preview.PreviewID || stalePreview.WorkspaceDigest == preview.WorkspaceDigest {
		t.Fatalf("checkout edit did not produce a fresh preview: old=%+v new=%+v err=%v", preview, stalePreview, err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("fresh preview silently reused the old user's resolution")
	}
	if err := os.WriteFile(checkoutFile, []byte(mergedValue), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err = manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.PreviewID == stalePreview.PreviewID {
		t.Fatalf("restored manual merge did not receive a new preview: %+v err=%v", preview, err)
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err != nil {
		t.Fatalf("resolve fresh manual merge: %v", err)
	}
	exported, err := manager.Export(ctx, scope, created.ID)
	if err != nil || exported.State != workspace.StateExported || exported.CandidateID == "" {
		t.Fatalf("export manually merged workspace=%+v err=%v", exported, err)
	}
	candidateRecord, err := state.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	candidateContent, err := os.ReadFile(filepath.Join(candidateRecord.Candidate.CandidateRoot, "board.txt"))
	if err != nil || string(candidateContent) != mergedValue {
		t.Fatalf("candidate content=%q err=%v; want exact manual merge %q", candidateContent, err, mergedValue)
	}
	formalContent, err := os.ReadFile(formalFile)
	if err != nil || string(formalContent) != formalSide {
		t.Fatalf("export changed formal content before review/accept: %q err=%v", formalContent, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("manual merge candidate review=%+v err=%v", review, err)
	}
	formalContent, err = os.ReadFile(formalFile)
	if err != nil || string(formalContent) != formalSide {
		t.Fatalf("review changed formal content before acceptance: %q err=%v", formalContent, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: exported.CandidateID, SessionID: session.ID, DecisionID: decisionID,
		PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest,
		FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	}); err != nil {
		t.Fatalf("accept reviewed manual merge candidate: %v", err)
	}
	formalContent, err = os.ReadFile(formalFile)
	if err != nil || string(formalContent) != mergedValue {
		t.Fatalf("accepted formal content=%q err=%v; want manual merge %q", formalContent, err, mergedValue)
	}
}
