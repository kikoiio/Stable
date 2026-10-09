package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A project-v2 acceptance journal without its captured metadata facts cannot
// safely complete the swap: recovery must preserve both roots and block before
// publishing a receipt.
func TestProjectAcceptanceRecoveryWithoutMetadataFactsBlocksAndRetainsRoots(t *testing.T) {
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
	for root, value := range map[string]string{formal: "old", incoming: "new"} {
		if err := os.WriteFile(filepath.Join(root, "board"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	metadataPath := filepath.Join(formal, ".stable", "session")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte("preserve metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataInfo, err := os.Lstat(filepath.Join(formal, ".stable"))
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
		ID: "missing-facts", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: formal, CandidateRoot: incoming,
		BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{
		ID: "missing-facts-decision", UserID: "user", CandidateID: c.ID,
		CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest,
		Mode: candidate.AcceptNormal,
	}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.ReconcileAcceptances(ctx); err == nil {
		t.Fatal("recovery without protected metadata facts unexpectedly succeeded")
	}
	assertRetained := func(attempt string) {
		t.Helper()
		for root, want := range map[string]string{formal: "old", incoming: "new"} {
			got, readErr := os.ReadFile(filepath.Join(root, "board"))
			if readErr != nil || string(got) != want {
				t.Errorf("%s root %s content=%q err=%v, want %q", attempt, root, got, readErr, want)
			}
		}
		info, statErr := os.Lstat(filepath.Join(formal, ".stable"))
		if statErr != nil || !os.SameFile(metadataInfo, info) {
			t.Errorf("%s formal .stable identity changed: got=%v err=%v", attempt, info, statErr)
		}
		data, readErr := os.ReadFile(metadataPath)
		if readErr != nil || string(data) != "preserve metadata" {
			t.Errorf("%s formal metadata=%q err=%v", attempt, data, readErr)
		}
		var phase string
		if queryErr := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); queryErr != nil || phase != "blocked" {
			t.Errorf("%s phase=%q err=%v, want blocked", attempt, phase, queryErr)
		}
		if _, ok, receiptErr := s.FindAcceptanceReceipt(ctx, d.ID); receiptErr != nil || ok {
			t.Errorf("%s receipt exists=%t err=%v", attempt, ok, receiptErr)
		}
	}
	assertRetained("first recovery")
	if err := s.ReconcileAcceptances(ctx); err != nil {
		t.Fatalf("repeat blocked recovery: %v", err)
	}
	assertRetained("repeat recovery")
}
