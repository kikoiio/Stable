package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/core"
)

func TestCandidateTransitionAndReviewPersistence(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	r := CandidateRecord{Candidate: candidate.Candidate{ID: "cand", FormalRoot: "/formal", CandidateRoot: "/candidate", BaselineDigest: "base", Status: "prepared"}, ActionID: "action", GoalID: "goal-1"}
	if err := s.SaveCandidate(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "prepared", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "running", "ready", "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "ready", "accepted", ""); err == nil {
		t.Fatal("invalid transition accepted")
	}
	loaded, err := s.GetCandidate(ctx, "cand")
	if err != nil || loaded.Candidate.CandidateDigest != "candidate" || loaded.Candidate.Status != "ready" {
		t.Fatalf("candidate=%+v err=%v", loaded, err)
	}
	review := candidate.Review{ID: "preview", CandidateID: "cand", FormalDigest: "base", CandidateDigest: "candidate", Digest: "preview-digest", Changes: []candidate.FileChange{{Path: "file", Status: "modified"}}}
	if err = s.SaveCandidateReview(ctx, review); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetCandidateReview(ctx, review.ID)
	if err != nil || again.Digest != review.Digest || len(again.Changes) != 1 {
		t.Fatalf("review=%+v err=%v", again, err)
	}
}

func TestAcceptanceFinalizeIsIdempotentAndQueuesReverification(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c := candidate.Candidate{ID: "cand", FormalRoot: "/formal", CandidateRoot: "/candidate", BaselineDigest: "old", CandidateDigest: "new", Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "decision", CandidateID: "cand", UserID: "local-user", CandidateDigest: "new", PreviewDigest: "preview", FormalDigest: "old", Mode: candidate.AcceptNormal}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAcceptancePhase(ctx, d.ID, "prepared", "swapped", ""); err != nil {
		t.Fatal(err)
	}
	receipt := candidate.Receipt{ID: "receipt", DecisionID: d.ID, CandidateID: c.ID, FormalDigest: "new", AcceptedAt: now}
	if err := s.FinalizeAcceptance(ctx, d, receipt, "goal-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeAcceptance(ctx, d, receipt, "goal-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAcceptanceReceipt(ctx, d.ID)
	if err != nil || got.ID != receipt.ID {
		t.Fatalf("receipt=%+v err=%v", got, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.CurrentArtifactID != "new" {
		t.Fatalf("goal not awaiting independent reverification: %+v", snapshot.Goal)
	}
	if len(snapshot.Events) != 1 || snapshot.Events[0].ID != "accept-decision" {
		t.Fatalf("acceptance wake events=%+v", snapshot.Events)
	}
}

func TestReconcilePreparedAcceptanceBeforeExchange(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(candRoot)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{ID: "reconcile", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "reconcile-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err = s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal after reconciliation=%q err=%v", data, err)
	}
	receipt, ok, err := s.FindAcceptanceReceipt(ctx, d.ID)
	if err != nil || !ok || receipt.FormalDigest != newDigest {
		t.Fatalf("receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
}

func TestReconcileAcceptanceAfterExchangeBeforeJournalUpdate(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600)
	_ = os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600)
	_, oldDigest, _ := candidate.BuildManifest(formal)
	_, newDigest, _ := candidate.BuildManifest(candRoot)
	c := candidate.Candidate{ID: "crash-after-exchange", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "crash-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptForce}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := candidate.ExchangeProjectDir(formal, candRoot); err != nil {
		t.Fatal(err)
	} // crash before phase becomes swapped
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "finalized" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
}

func TestReconcileAcceptanceConflictBlocks(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600)
	_ = os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600)
	_, oldDigest, _ := candidate.BuildManifest(formal)
	_, newDigest, _ := candidate.BuildManifest(candRoot)
	c := candidate.Candidate{ID: "conflict", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "conflict-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("external"), 0600)
	if err := s.ReconcileAcceptances(ctx); err == nil {
		t.Fatal("digest conflict did not block")
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
}

func TestAcceptCandidateUsesDurableJournal(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := candidate.CreateCandidate("durable", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = candidate.FreezeCandidate(c, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "reviewed"
	review, err := candidate.BuildReview(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "durable-decision", UserID: "local-user", CandidateID: c.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: candidate.AcceptNormal}
	receipt, err := candidate.AcceptCandidate(ctx, c, review, d, "goal-1", "", s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	second, err := candidate.AcceptCandidate(ctx, c, review, d, "goal-1", "", s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != receipt.ID {
		t.Fatalf("retry changed receipt: %+v != %+v", second, receipt)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal project after acceptance=%q err=%v", data, err)
	}
}
