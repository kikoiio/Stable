package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A crash after the database finalizes a rewind but before transaction cleanup
// must not strand the old candidate root in the staging path forever.
func TestReconcileFinalizedRewindCleansPostFinalizeStaging(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "candidate")
	staging := filepath.Join(parent, "staging")
	rollback := staging + ".rollback"
	for path, contents := range map[string]string{root: "old", staging: "new"} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "board"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, expectedDigest, err := candidate.BuildManifestForPolicy(root, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{
		ID: "rewind-cleanup", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: t.TempDir(), CandidateRoot: root,
		BaselineDigest: expectedDigest, CandidateDigest: expectedDigest, Status: "ready",
	}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{
		ID: "rw-cleanup", CandidateID: c.ID, SnapshotID: "snapshot",
		ExpectedDigest: expectedDigest, TargetDigest: targetDigest,
		StagingDir: staging, RollbackPath: rollback, TransactionMode: "journaled-move",
		ManifestPolicy: candidate.ManifestPolicyProject,
	}
	if err = s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	items, err := s.UnfinishedRewinds(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("unfinished journals=%d err=%v", len(items), err)
	}
	j = items[0]
	tx := candidate.DirectoryTransaction{
		ID: j.ID, Kind: candidate.TransactionRewind, ManifestPolicy: j.ManifestPolicy,
		ExpectedRootIdentity: j.ExpectedRootIdentity, TargetRootIdentity: j.TargetRootIdentity,
		CurrentRoot: root, IncomingRoot: staging, RollbackRoot: rollback,
		ExpectedDigest: expectedDigest, TargetDigest: targetDigest, Mode: j.TransactionMode,
	}
	if err = candidate.NewTransactionCoordinator().Apply(ctx, tx, rewindJournalAdapter{store: s}); err != nil {
		t.Fatalf("apply rewind: %v", err)
	}
	j.Phase = RewindSwapped
	if err = s.FinalizeRewind(ctx, j); err != nil {
		t.Fatalf("finalize rewind: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ReconcileRewinds(ctx); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if _, err = os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatalf("post-finalize staging was not cleaned: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("rewound candidate=%q err=%v", data, err)
	}
}
