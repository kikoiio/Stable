package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A process can stop after an atomic exchange and protected-metadata restore
// have completed but before SQLite records a receipt. Recovery must recognize
// the installed project-v2 tree, finalize once, and clean the spent root.
func TestProjectAcceptanceRecoversAtomicMetadataRestoreBeforeReceipt(t *testing.T) {
	ctx := context.Background()
	s, dbPath := newGoalStore(t)
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	incoming := filepath.Join(parent, "incoming")
	for _, root := range []string{formal, incoming} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for root, value := range map[string]string{formal: "old project", incoming: "accepted project"} {
		if err := os.WriteFile(filepath.Join(root, "board.txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	metadata := map[string]string{
		".git":                "gitdir: /private/common/worktrees/formal\n",
		".stable/session":     "live session metadata",
		".mewcode/legacy.log": "legacy runtime metadata",
	}
	metadataPaths := map[string]string{
		".git":                filepath.Join(formal, ".git"),
		".stable/session":     filepath.Join(formal, ".stable"),
		".mewcode/legacy.log": filepath.Join(formal, ".mewcode"),
	}
	metadataInfo := make(map[string]os.FileInfo, len(metadata))
	for name, value := range metadata {
		path := filepath.Join(formal, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range metadataPaths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		metadataInfo[name] = info
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
		ID: "atomic-metadata-before-receipt", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: formal, CandidateRoot: incoming, BaselineDigest: oldDigest,
		CandidateDigest: newDigest, Status: "reviewed",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "atomic-metadata-before-receipt-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{
		ID: "atomic-metadata-before-receipt-decision", UserID: "user", CandidateID: c.ID,
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
	if err := candidate.ExchangeProjectDir(formal, incoming); err != nil {
		t.Fatal(err)
	}
	if err := candidate.RestoreProtectedMetadataFacts(formal, incoming, facts); err != nil {
		t.Fatalf("simulate metadata restore before process exit: %v", err)
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "prepared" {
		t.Fatalf("pre-restart phase=%q err=%v; want prepared before receipt", phase, err)
	}
	if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
		t.Fatalf("receipt unexpectedly exists before recovery: ok=%t err=%v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.ReconcileAcceptances(ctx); err != nil {
			t.Fatalf("recovery attempt %d: %v", attempt+1, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(formal, "board.txt")); err != nil || string(got) != "accepted project" {
		t.Fatalf("accepted formal bytes=%q err=%v", got, err)
	}
	for name, want := range metadata {
		path := filepath.Join(formal, name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("protected metadata %s=%q want=%q err=%v", name, got, want, err)
		}
		identityPath := metadataPaths[name]
		info, statErr := os.Lstat(identityPath)
		if statErr != nil || !os.SameFile(metadataInfo[name], info) {
			t.Fatalf("protected metadata identity %s changed: before=%v after=%v err=%v", identityPath, metadataInfo[name], info, statErr)
		}
	}
	if _, err := os.Lstat(incoming); !os.IsNotExist(err) {
		t.Fatalf("spent incoming root was not cleaned after recovery: %v", err)
	}
	var receiptCount int
	if err := s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("receipt count=%d err=%v; want exactly one", receiptCount, err)
	}
	if receipt, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || !ok || receipt.CandidateID != c.ID || receipt.FormalDigest != newDigest {
		t.Fatalf("receipt=%+v exists=%t err=%v", receipt, ok, err)
	}
}
