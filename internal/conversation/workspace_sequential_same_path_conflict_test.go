package conversation

import (
	"context"
	"database/sql"
	"errors"
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

func TestSequentialWorkspacesRequireResolutionForSamePathConflict(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "board.txt")
	if err := os.WriteFile(formalFile, []byte("original baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "sequential same path conflict")
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
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "samepath")
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
	scope := workspace.Scope{
		ProjectID: "samepath", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "same-path-origin", SessionID: session.ID,
			AllowedRoot: formalAbs, FormalRoot: formalAbs,
			CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	first, err := manager.Create(ctx, scope, "first same-path workspace")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, scope, "second same-path workspace")
	if err != nil {
		t.Fatal(err)
	}
	firstPaths, err := layout.Paths(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondPaths, err := layout.Paths(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	const firstValue = "accepted from first workspace"
	const secondValue = "second workspace edit"
	if err := os.WriteFile(filepath.Join(firstPaths.Checkout, "board.txt"), []byte(firstValue), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondPaths.Checkout, "board.txt"), []byte(secondValue), 0600); err != nil {
		t.Fatal(err)
	}
	firstExport, err := manager.Export(ctx, scope, first.ID)
	if err != nil || firstExport.CandidateID == "" {
		t.Fatalf("first workspace export=%+v err=%v", firstExport, err)
	}
	firstReview, err := service.reviewCandidate(ctx, firstExport.CandidateID, session.ID)
	if err != nil || firstReview.CandidateDigest == "" || firstReview.Digest == "" {
		t.Fatalf("first workspace review=%+v err=%v", firstReview, err)
	}
	firstDecisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: firstExport.CandidateID, SessionID: session.ID, DecisionID: firstDecisionID,
		PreviewDigest: firstReview.Digest, CandidateDigest: firstReview.CandidateDigest,
		FormalDigest: firstReview.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || firstReceipt.ID == "" {
		t.Fatalf("accept first workspace candidate: receipt=%+v err=%v", firstReceipt, err)
	}
	if err := service.replayAcceptedWorkspaceRoots(ctx, manager, formalAbs); err != nil {
		t.Fatalf("rebind older workspace after formal acceptance: %v", err)
	}
	assertContent := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v; want %q", path, got, err, want)
		}
	}
	assertContent(formalFile, firstValue)
	assertContent(filepath.Join(secondPaths.Baseline, "board.txt"), "original baseline")

	preview, err := manager.Preview(ctx, scope, second.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" || preview.PreviewID == "" {
		t.Fatalf("second workspace same-path conflict preview=%+v err=%v", preview, err)
	}
	if _, err := manager.Export(ctx, scope, second.ID); err == nil {
		t.Fatal("unresolved same-path conflict exported a candidate")
	}
	blocked, err := manager.Get(ctx, scope, second.ID)
	if err != nil || blocked.CandidateID != "" || blocked.ResolvedCount != 0 {
		t.Fatalf("unresolved export changed second workspace decision state: snapshot=%+v err=%v", blocked, err)
	}
	if _, err := db.GetCandidate(ctx, second.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unresolved second workspace created a candidate under its workspace ID: %v", err)
	}
	// No reviewable candidate exists for the unresolved second workspace, so an
	// explicit acceptance attempt using its workspace ID must fail as well.
	if _, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: second.ID, SessionID: session.ID, DecisionID: "unresolved-workspace-decision",
		PreviewDigest: "preview", CandidateDigest: "candidate", FormalDigest: "formal",
		AcceptanceMode: string(candidate.AcceptNormal),
	}); err == nil {
		t.Fatal("acceptance succeeded without a candidate exported from the unresolved workspace")
	}
	assertContent(formalFile, firstValue)
	assertContent(filepath.Join(secondPaths.Baseline, "board.txt"), "original baseline")

	resolved, err := manager.ResolveUser(ctx, scope, second.ID, "user", preview.PreviewID, preview.Generation,
		map[string]string{"board.txt": workspace.UseWorkspace})
	if err != nil || resolved.ResolvedCount != 1 {
		t.Fatalf("explicitly resolve second workspace choice: snapshot=%+v err=%v", resolved, err)
	}
	secondExport, err := manager.Export(ctx, scope, second.ID)
	if err != nil || secondExport.CandidateID == "" {
		t.Fatalf("export resolved second workspace: snapshot=%+v err=%v", secondExport, err)
	}
	secondRecord, err := db.GetCandidate(ctx, secondExport.CandidateID)
	if err != nil || secondRecord.Candidate.Status != "ready" {
		t.Fatalf("resolved second candidate=%+v err=%v", secondRecord.Candidate, err)
	}
	assertContent(filepath.Join(secondRecord.Candidate.CandidateRoot, "board.txt"), secondValue)
	assertContent(formalFile, firstValue)
	assertContent(filepath.Join(secondPaths.Baseline, "board.txt"), "original baseline")
	secondReview, err := service.reviewCandidate(ctx, secondExport.CandidateID, session.ID)
	if err != nil || secondReview.CandidateDigest == "" || secondReview.Digest == "" {
		t.Fatalf("resolved second review=%+v err=%v", secondReview, err)
	}
	secondDecisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: secondExport.CandidateID, SessionID: session.ID, DecisionID: secondDecisionID,
		PreviewDigest: secondReview.Digest, CandidateDigest: secondReview.CandidateDigest,
		FormalDigest: secondReview.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || secondReceipt.ID == "" {
		t.Fatalf("accept resolved second workspace candidate: receipt=%+v err=%v", secondReceipt, err)
	}
	assertContent(formalFile, secondValue)
	assertContent(filepath.Join(secondPaths.Baseline, "board.txt"), "original baseline")
}
