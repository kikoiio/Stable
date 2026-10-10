package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceSocketManualConflictResolutionExportsExactMergedValue(t *testing.T) {
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
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	socketDir, err := os.MkdirTemp(os.TempDir(), "m09-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "conversation.sock")
	svc, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal,
		WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath:         socket, PollEvery: time.Hour,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sessions, err := Request(reqctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "manual-conflict-origin", SessionID: sessionID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := svc.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "manual conflict merge")
	if err != nil {
		t.Fatal(err)
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
	baselineFile := filepath.Join(paths.Baseline, "board.txt")
	const formalValue = "formal edit"
	const mergedValue = "manual merge of baseline, formal, and workspace edits"
	if err := os.WriteFile(formalFile, []byte(formalValue), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte(mergedValue), 0600); err != nil {
		t.Fatal(err)
	}
	previewMessages, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID})
	if err != nil || len(previewMessages) != 1 || previewMessages[0].Worktree == nil {
		t.Fatalf("preview manual conflict: messages=%+v err=%v", previewMessages, err)
	}
	preview := previewMessages[0].Worktree
	if preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("unexpected manual conflict preview: %+v", preview)
	}
	resolved, err := Request(reqctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"board.txt": workspace.UseWorkspace},
	})
	if err != nil || len(resolved) != 1 || resolved[0].Worktree == nil || resolved[0].Worktree.ResolvedCount != 1 {
		t.Fatalf("resolve exact manual merge path: messages=%+v err=%v", resolved, err)
	}
	exported, err := Request(reqctx, socket, ClientMsg{Op: "worktree_export", SessionID: sessionID, ID: created.ID})
	if err != nil || len(exported) != 1 || exported[0].Worktree == nil || exported[0].Worktree.CandidateID == "" {
		t.Fatalf("export manually resolved candidate: messages=%+v err=%v", exported, err)
	}
	candidateID := exported[0].Worktree.CandidateID
	record, err := db.GetCandidate(ctx, candidateID)
	if err != nil || record.Candidate.Status != "ready" {
		t.Fatalf("candidate=%+v err=%v; want ready", record, err)
	}
	candidateBaselineDigest := record.Candidate.BaselineDigest
	if candidateBaselineDigest == "" {
		t.Fatal("exported candidate has no baseline identity")
	}
	gotCandidate, err := os.ReadFile(filepath.Join(record.Candidate.CandidateRoot, "board.txt"))
	if err != nil || string(gotCandidate) != mergedValue {
		t.Fatalf("candidate content=%q err=%v; want exact manual merge %q", gotCandidate, err, mergedValue)
	}
	if gotFormal, err := os.ReadFile(formalFile); err != nil || string(gotFormal) != formalValue {
		t.Fatalf("export changed formal content: %q err=%v", gotFormal, err)
	}
	if gotBaseline, err := os.ReadFile(baselineFile); err != nil || string(gotBaseline) != "baseline" {
		t.Fatalf("export changed baseline content: %q err=%v", gotBaseline, err)
	}
	review, err := svc.reviewCandidate(ctx, candidateID, sessionID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" {
		t.Fatalf("candidate review=%+v err=%v", review, err)
	}
	if gotFormal, err := os.ReadFile(formalFile); err != nil || string(gotFormal) != formalValue {
		t.Fatalf("review changed formal content: %q err=%v", gotFormal, err)
	}
	if gotBaseline, err := os.ReadFile(baselineFile); err != nil || string(gotBaseline) != "baseline" {
		t.Fatalf("review changed baseline content: %q err=%v", gotBaseline, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := Request(reqctx, socket, ClientMsg{
		Op: "review_accept", SessionID: sessionID, CandidateID: candidateID,
		DecisionID: decisionID, PreviewDigest: review.Digest,
		CandidateDigest: review.CandidateDigest, FormalDigest: review.FormalDigest,
		AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || len(accepted) != 1 || accepted[0].Type != "acceptance" || accepted[0].Receipt == nil {
		t.Fatalf("explicit socket acceptance: messages=%+v err=%v", accepted, err)
	}
	receipt := accepted[0].Receipt
	if receipt.ID == "" || receipt.DecisionID != decisionID || receipt.CandidateID != candidateID || receipt.FormalDigest != review.CandidateDigest {
		t.Fatalf("acceptance receipt does not match candidate/review identity: receipt=%+v review=%+v", receipt, review)
	}
	acceptedRecord, err := db.GetCandidate(ctx, candidateID)
	if err != nil || acceptedRecord.Candidate.ID != candidateID || acceptedRecord.Candidate.Status != "accepted" || acceptedRecord.Candidate.BaselineDigest != candidateBaselineDigest || acceptedRecord.Candidate.CandidateDigest != receipt.FormalDigest {
		t.Fatalf("accepted candidate identity=%+v err=%v; want same candidate, baseline %q and receipt digest %q", acceptedRecord, err, candidateBaselineDigest, receipt.FormalDigest)
	}
	savedReview, err := db.GetCandidateReview(ctx, review.ID)
	if err != nil || savedReview.ID != review.ID || savedReview.CandidateID != candidateID || savedReview.Digest != review.Digest || savedReview.CandidateDigest != review.CandidateDigest || savedReview.FormalDigest != review.FormalDigest {
		t.Fatalf("acceptance changed or lost review identity: saved=%+v original=%+v err=%v", savedReview, review, err)
	}
	if gotFormal, err := os.ReadFile(formalFile); err != nil || string(gotFormal) != mergedValue {
		t.Fatalf("accepted formal content=%q err=%v; want manual merge %q", gotFormal, err, mergedValue)
	}
	if gotBaseline, err := os.ReadFile(baselineFile); err != nil || string(gotBaseline) != "baseline" {
		t.Fatalf("acceptance changed workspace baseline: %q err=%v", gotBaseline, err)
	}
}
