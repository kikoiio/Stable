package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// An atomic directory exchange can complete before its phase update reaches
// the journal. Project-v2 recovery must restore the original protected
// metadata and finalize exactly one receipt after reopening the store.
func TestReconcileAtomicExchangeRestoresProjectMetadataAfterPreparedCrash(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	candidateRoot := filepath.Join(parent, "candidate")
	for _, root := range []string{formal, candidateRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for root, value := range map[string]string{formal: "old project", candidateRoot: "accepted project"} {
		if err := os.WriteFile(filepath.Join(root, "board.txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}

	gitPointerPath := filepath.Join(formal, ".git")
	gitPointerBytes := []byte("gitdir: /private/common/worktrees/formal\n")
	if err := os.WriteFile(gitPointerPath, gitPointerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	for name, marker := range map[string]string{
		".stable/session":  "live session metadata",
		".mewcode/history": "legacy runtime metadata",
	} {
		path := filepath.Join(formal, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	metadataPaths := []string{gitPointerPath, filepath.Join(formal, ".stable"), filepath.Join(formal, ".mewcode")}
	metadataInfo := make(map[string]os.FileInfo, len(metadataPaths))
	for _, path := range metadataPaths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		metadataInfo[path] = info
	}

	_, oldDigest, err := candidate.BuildManifestForPolicy(formal, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifestForPolicy(candidateRoot, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{
		ID: "atomic-metadata-recovery", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: formal, CandidateRoot: candidateRoot,
		BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "atomic-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{
		ID: "atomic-metadata-decision", UserID: "user", CandidateID: c.ID,
		CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest,
		Mode: candidate.AcceptNormal,
	}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	mode, _, err := s.AcceptanceTransaction(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "atomic-exchange" {
		t.Skipf("platform transaction mode %q does not exercise atomic exchange", mode)
	}
	facts, err := candidate.CaptureProtectedMetadata(formal)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProtectedMetadata(ctx, d.ID, facts); err != nil {
		t.Fatal(err)
	}

	// Model process loss after the filesystem syscall and before the journal
	// advances from prepared to swapped.
	if err := candidate.ExchangeProjectDir(formal, candidateRoot); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "prepared" {
		t.Fatalf("pre-restart journal phase=%q err=%v; want prepared", phase, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for attempt := 0; attempt < 2; attempt++ {
		if err := s.ReconcileAcceptances(ctx); err != nil {
			t.Fatalf("reconcile attempt %d: %v", attempt+1, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(formal, "board.txt")); err != nil || string(got) != "accepted project" {
		t.Fatalf("accepted formal content=%q err=%v", got, err)
	}
	for _, path := range metadataPaths {
		gotInfo, err := os.Lstat(path)
		if err != nil || !os.SameFile(metadataInfo[path], gotInfo) {
			t.Fatalf("protected metadata identity at %s changed: got=%v err=%v", path, gotInfo, err)
		}
	}
	if got, err := os.ReadFile(gitPointerPath); err != nil || string(got) != string(gitPointerBytes) {
		t.Fatalf("linked Git pointer=%q err=%v", got, err)
	}
	for name, want := range map[string]string{
		".stable/session":  "live session metadata",
		".mewcode/history": "legacy runtime metadata",
	} {
		if got, err := os.ReadFile(filepath.Join(formal, name)); err != nil || string(got) != want {
			t.Fatalf("protected metadata %s=%q err=%v", name, got, err)
		}
	}
	if err := candidate.ValidateProtectedMetadataFacts(formal, candidateRoot, facts); err != nil {
		t.Fatalf("protected metadata facts after recovery: %v", err)
	}
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "finalized" {
		t.Fatalf("journal phase=%q err=%v; want finalized", phase, err)
	}
	if got, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || !ok || got.CandidateID != c.ID || got.FormalDigest != newDigest {
		t.Fatalf("receipt=%+v exists=%t err=%v", got, ok, err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v; want exactly one", count, err)
	}
}
