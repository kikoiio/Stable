package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
)

// A crash after acceptance finalization but before transaction cleanup must
// recover the unique receipt while removing only the identity-checked spent
// candidate root.
func TestReconcileFinalizedAcceptanceCleansPostFinalizeCandidateRoot(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	candidateRoot := filepath.Join(parent, "candidate")
	for path, contents := range map[string]string{formal: "old", candidateRoot: "new"} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "board"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
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
		ID: "accept-cleanup", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: formal, CandidateRoot: candidateRoot,
		BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed",
	}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{
		ID: "decision-cleanup", UserID: "user", CandidateID: c.ID,
		CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest,
		Mode: candidate.AcceptForce,
	}
	if _, err = s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	facts, err := candidate.CaptureProtectedMetadata(formal)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveProtectedMetadata(ctx, d.ID, facts); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(parent, "rollback")
	if _, err = s.DB().Exec(`UPDATE acceptance_apply_journal SET transaction_mode='journaled-move',rollback_path=? WHERE decision_id=?`, rollback, d.ID); err != nil {
		t.Fatal(err)
	}
	var expectedIdentity, targetIdentity string
	if err = s.DB().QueryRow(`SELECT expected_root_identity,target_root_identity FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&expectedIdentity, &targetIdentity); err != nil {
		t.Fatal(err)
	}
	tx := candidate.DirectoryTransaction{
		ID: d.ID, Kind: candidate.TransactionAcceptance, ManifestPolicy: candidate.ManifestPolicyProject,
		ProtectedMetadata: facts, ExpectedRootIdentity: expectedIdentity, TargetRootIdentity: targetIdentity,
		CurrentRoot: formal, IncomingRoot: candidateRoot, RollbackRoot: rollback,
		ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "journaled-move",
	}
	if err = candidate.NewTransactionCoordinator().Apply(ctx, tx, acceptanceJournalAdapter{store: s}); err != nil {
		t.Fatalf("apply acceptance: %v", err)
	}
	if err = candidate.RestoreProtectedMetadataFacts(formal, candidateRoot, facts); err != nil {
		t.Fatalf("restore protected metadata: %v", err)
	}
	state, err := candidate.NewTransactionCoordinator().Inspect(ctx, tx, candidate.PhaseSwapped)
	if err != nil || state != candidate.RecoveryNew {
		t.Fatalf("acceptance state=%s err=%v", state, err)
	}
	receipt := candidate.Receipt{
		ID: "receipt-" + d.ID, DecisionID: d.ID, CandidateID: c.ID,
		FormalDigest: newDigest, AcceptedAt: time.Now().UTC(),
	}
	if err = s.FinalizeAcceptance(ctx, d, receipt, "goal-1", "act"); err != nil {
		t.Fatalf("finalize acceptance: %v", err)
	}
	// Simulate process loss here: the receipt is committed, cleanup has not run.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if _, err = os.Lstat(candidateRoot); !os.IsNotExist(err) {
		t.Fatalf("spent candidate root was not cleaned: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal current root=%q err=%v", data, err)
	}
	rec, err := s.GetCandidate(ctx, c.ID)
	if err != nil || rec.Candidate.Status != "accepted" || rec.Candidate.CandidateDigest != newDigest {
		t.Fatalf("candidate after acceptance recovery: %+v err=%v", rec.Candidate, err)
	}
	gotReceipt, ok, err := s.FindAcceptanceReceipt(ctx, d.ID)
	if err != nil || !ok || gotReceipt.ID != receipt.ID || gotReceipt.CandidateID != c.ID || gotReceipt.FormalDigest != newDigest {
		t.Fatalf("receipt=%+v ok=%v err=%v", gotReceipt, ok, err)
	}
}
