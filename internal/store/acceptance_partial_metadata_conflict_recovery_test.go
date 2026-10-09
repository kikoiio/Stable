package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A crash after restoring .git but before restoring .stable can leave a later
// metadata destination occupied by an unknown writer. Recovery must retain the
// already-restored identity, the unknown replacement, and all remaining
// service metadata, then stay blocked on repeated startup reconciliation.
func TestReconcileAcceptanceRetainsPartialMetadataRestoreConflict(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	incoming := filepath.Join(parent, "incoming")
	for _, root := range []string{formal, incoming} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		".git/config":          "formal git config",
		".stable/session.json": "live session log",
		".mewcode/history":     "legacy history",
	}
	for name, content := range metadata {
		path := filepath.Join(formal, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitBefore, err := os.Lstat(filepath.Join(formal, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	stableBefore, err := os.Lstat(filepath.Join(formal, ".stable"))
	if err != nil {
		t.Fatal(err)
	}
	mewcodeBefore, err := os.Lstat(filepath.Join(formal, ".mewcode"))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := candidate.CaptureProtectedMetadata(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifestForPolicy(formal, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifestForPolicy(incoming, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{
		ID: "partial-metadata-conflict", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: formal, CandidateRoot: incoming, BaselineDigest: oldDigest,
		CandidateDigest: newDigest, Status: "reviewed",
	}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{
		ID: "partial-metadata-conflict-decision", UserID: "user", CandidateID: c.ID,
		CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest,
		Mode: candidate.AcceptNormal,
	}
	if _, err = s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProtectedMetadata(ctx, d.ID, facts); err != nil {
		t.Fatal(err)
	}
	mode, rollback, err := s.AcceptanceTransaction(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectedIdentity, err := candidate.CaptureRootIdentity(formal)
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, err := candidate.CaptureRootIdentity(incoming)
	if err != nil {
		t.Fatal(err)
	}
	tx := candidate.DirectoryTransaction{
		ID: d.ID, Kind: candidate.TransactionAcceptance,
		ManifestPolicy: candidate.ManifestPolicyProject, ProtectedMetadata: facts,
		CurrentRoot: formal, IncomingRoot: incoming, RollbackRoot: rollback,
		ExpectedRootIdentity: expectedIdentity, TargetRootIdentity: targetIdentity,
		ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: mode,
	}
	if err = candidate.NewTransactionCoordinator().Apply(ctx, tx, acceptanceJournalAdapter{store: s}); err != nil {
		t.Fatalf("simulate completed root exchange: %v", err)
	}

	// Simulate process loss during metadata restore: .git has already returned
	// to formal, while an unknown .stable destination appears before its restore.
	if err = os.Rename(filepath.Join(incoming, ".git"), filepath.Join(formal, ".git")); err != nil {
		t.Fatal(err)
	}
	conflictStable := filepath.Join(formal, ".stable")
	if err = os.Mkdir(conflictStable, 0700); err != nil {
		t.Fatal(err)
	}
	conflictFile := filepath.Join(conflictStable, "external-owner")
	if err = os.WriteFile(conflictFile, []byte("unknown replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	conflictBefore, err := os.Lstat(conflictStable)
	if err != nil {
		t.Fatal(err)
	}
	conflictFileBefore, err := os.Lstat(conflictFile)
	if err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(formal, ".git", "config")
	gitConfigBefore, err := os.Lstat(gitConfig)
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
	defer s.Close()
	if err = s.ReconcileAcceptances(ctx); err == nil {
		t.Fatal("recovery accepted an unknown .stable destination")
	}
	assertUnchanged := func(attempt string) {
		t.Helper()
		if data, readErr := os.ReadFile(filepath.Join(formal, "board")); readErr != nil || string(data) != "new" {
			t.Errorf("%s formal content=%q err=%v", attempt, data, readErr)
		}
		if data, readErr := os.ReadFile(gitConfig); readErr != nil || string(data) != metadata[".git/config"] {
			t.Errorf("%s restored .git bytes=%q err=%v", attempt, data, readErr)
		}
		if info, statErr := os.Lstat(filepath.Join(formal, ".git")); statErr != nil || !os.SameFile(gitBefore, info) {
			t.Errorf("%s restored .git inode changed: before=%v after=%v err=%v", attempt, gitBefore, info, statErr)
		}
		if info, statErr := os.Lstat(gitConfig); statErr != nil || !os.SameFile(gitConfigBefore, info) {
			t.Errorf("%s restored .git config inode changed: before=%v after=%v err=%v", attempt, gitConfigBefore, info, statErr)
		}
		if data, readErr := os.ReadFile(conflictFile); readErr != nil || string(data) != "unknown replacement" {
			t.Errorf("%s unknown .stable bytes=%q err=%v", attempt, data, readErr)
		}
		if info, statErr := os.Lstat(conflictStable); statErr != nil || !os.SameFile(conflictBefore, info) {
			t.Errorf("%s unknown .stable inode changed: before=%v after=%v err=%v", attempt, conflictBefore, info, statErr)
		}
		if info, statErr := os.Lstat(conflictFile); statErr != nil || !os.SameFile(conflictFileBefore, info) {
			t.Errorf("%s unknown .stable file inode changed: before=%v after=%v err=%v", attempt, conflictFileBefore, info, statErr)
		}
		for _, name := range []string{".stable/session.json", ".mewcode/history"} {
			path := filepath.Join(incoming, name)
			if data, readErr := os.ReadFile(path); readErr != nil || string(data) != metadata[name] {
				t.Errorf("%s retained %s bytes=%q err=%v", attempt, name, data, readErr)
			}
		}
		if info, statErr := os.Lstat(filepath.Join(incoming, ".stable")); statErr != nil || !os.SameFile(stableBefore, info) {
			t.Errorf("%s retained .stable inode changed: before=%v after=%v err=%v", attempt, stableBefore, info, statErr)
		}
		if info, statErr := os.Lstat(filepath.Join(incoming, ".mewcode")); statErr != nil || !os.SameFile(mewcodeBefore, info) {
			t.Errorf("%s retained .mewcode inode changed: before=%v after=%v err=%v", attempt, mewcodeBefore, info, statErr)
		}
		var phase string
		if queryErr := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); queryErr != nil || phase != "blocked" {
			t.Errorf("%s phase=%q err=%v, want blocked", attempt, phase, queryErr)
		}
		if _, ok, receiptErr := s.FindAcceptanceReceipt(ctx, d.ID); receiptErr != nil || ok {
			t.Errorf("%s receipt exists=%t err=%v", attempt, ok, receiptErr)
		}
	}
	assertUnchanged("first recovery")
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatalf("repeat recovery should leave blocked transaction untouched: %v", err)
	}
	assertUnchanged("repeated recovery")
}
