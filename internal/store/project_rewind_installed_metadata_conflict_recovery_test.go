package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A project-v2 rewind may crash after installing the snapshot but before the
// journal records that move. If protected metadata appears in that installed
// root, recovery must fail closed and preserve both roots across retries.
func TestProjectRewindRecoveryRetainsInstalledRootWithUnexpectedMetadata(t *testing.T) {
	ctx := context.Background()
	s, dbPath := newGoalStore(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "candidate")
	staging := filepath.Join(parent, "staging")
	rollback := filepath.Join(parent, "rollback")
	for path, contents := range map[string]string{root: "current candidate", staging: "rewind snapshot"} {
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
		ID: "project-rewind-installed-metadata", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: filepath.Join(parent, "formal"), CandidateRoot: root,
		BaselineDigest: expectedDigest, CandidateDigest: expectedDigest, Status: "ready",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "project-rewind-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{
		ID: "project-rewind-installed-metadata-journal", CandidateID: c.ID, SnapshotID: "snapshot",
		ExpectedDigest: expectedDigest, TargetDigest: targetDigest, StagingDir: staging,
		RollbackPath: rollback, TransactionMode: "journaled-move", ManifestPolicy: candidate.ManifestPolicyProject,
	}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	// Simulate a process exit after both renames but before the phase update.
	if err := os.Rename(root, rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE rewind_journal SET phase=? WHERE id=?`, RewindOldSaved, j.ID); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, ".stable", "unexpected-session")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte("retain unexpected protected data"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataInfo, err := os.Lstat(metadata)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	rollbackInfo, err := os.Lstat(rollback)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.ReconcileRewinds(ctx); err == nil {
		t.Fatal("project-v2 rewind recovered an installed root containing unexpected protected metadata")
	}
	assertRetained := func(attempt string) {
		t.Helper()
		for path, want := range map[string]string{
			filepath.Join(root, "board"):     "rewind snapshot",
			filepath.Join(rollback, "board"): "current candidate",
			metadata:                         "retain unexpected protected data",
		} {
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != want {
				t.Errorf("%s changed %s: got=%q want=%q err=%v", attempt, path, got, want, readErr)
			}
		}
		for path, before := range map[string]os.FileInfo{root: rootInfo, rollback: rollbackInfo, metadata: metadataInfo} {
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) {
				t.Errorf("%s identity changed at %s: before=%v after=%v err=%v", attempt, path, before, after, statErr)
			}
		}
		var phase string
		if queryErr := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&phase); queryErr != nil || phase != RewindBlocked {
			t.Errorf("%s journal phase=%q err=%v; want blocked", attempt, phase, queryErr)
		}
		record, getErr := s.GetCandidate(ctx, c.ID)
		if getErr != nil || record.Candidate.Status != "ready" || record.Candidate.CandidateDigest != expectedDigest {
			t.Errorf("%s candidate changed: %+v err=%v", attempt, record.Candidate, getErr)
		}
	}
	assertRetained("first recovery")
	if err := s.ReconcileRewinds(ctx); err != nil {
		t.Fatalf("repeat blocked recovery: %v", err)
	}
	assertRetained("repeat recovery")
}
