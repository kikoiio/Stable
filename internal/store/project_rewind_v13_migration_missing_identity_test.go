package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// v13 already distinguishes project-v2 manifests, but predates persisted
// transaction root identities. Migration must preserve that policy without
// guessing identities for an unfinished rewind.
func TestProjectRewindV13MigrationBlocksMissingRootIdentity(t *testing.T) {
	ctx := context.Background()
	s, dbPath := newGoalStore(t)
	parent := t.TempDir()
	current := filepath.Join(parent, "candidate")
	staging := filepath.Join(parent, "staging")
	for path, contents := range map[string]string{current: "current project candidate", staging: "project snapshot"} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "board.txt"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	identities := make(map[string]os.FileInfo)
	for _, path := range []string{current, staging, filepath.Join(current, "board.txt"), filepath.Join(staging, "board.txt")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		identities[path] = info
	}
	_, expectedDigest, err := candidate.BuildManifestForPolicy(current, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{
		ID: "project-v13-rewind", ManifestPolicy: candidate.ManifestPolicyProject,
		FormalRoot: filepath.Join(parent, "formal"), CandidateRoot: current,
		BaselineDigest: expectedDigest, CandidateDigest: expectedDigest, Status: "ready",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "project-v13-rewind-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{
		ID: "project-v13-rewind-journal", CandidateID: c.ID, SnapshotID: "project-snapshot",
		ExpectedDigest: expectedDigest, TargetDigest: targetDigest, StagingDir: staging,
		ManifestPolicy: candidate.ManifestPolicyProject,
	}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`
		ALTER TABLE rewind_journal DROP COLUMN expected_root_identity;
		ALTER TABLE rewind_journal DROP COLUMN target_root_identity;
		PRAGMA user_version=13;
	`); err != nil {
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
	var version int
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 14 {
		t.Fatalf("schema version=%d err=%v; want migrated v14", version, err)
	}
	var candidatePolicy, journalPolicy, expectedIdentity, targetIdentity, phase string
	if err := s.DB().QueryRow(`SELECT manifest_policy FROM candidates WHERE id=?`, c.ID).Scan(&candidatePolicy); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT manifest_policy,expected_root_identity,target_root_identity,phase FROM rewind_journal WHERE id=?`, j.ID).
		Scan(&journalPolicy, &expectedIdentity, &targetIdentity, &phase); err != nil {
		t.Fatal(err)
	}
	if candidatePolicy != candidate.ManifestPolicyProject || journalPolicy != candidate.ManifestPolicyProject || expectedIdentity != "" || targetIdentity != "" || phase != RewindPrepared {
		t.Fatalf("migrated project rewind changed contract: candidate policy=%q journal policy=%q identities=%q/%q phase=%q", candidatePolicy, journalPolicy, expectedIdentity, targetIdentity, phase)
	}
	if err := s.ReconcileRewinds(ctx); err == nil {
		t.Fatal("project-v2 rewind with identities absent in v13 was silently resumed")
	}
	assertRetained := func(attempt string) {
		t.Helper()
		for path, want := range map[string]string{
			filepath.Join(current, "board.txt"): "current project candidate",
			filepath.Join(staging, "board.txt"): "project snapshot",
		} {
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != want {
				t.Errorf("%s changed %s: got=%q want=%q err=%v", attempt, path, got, want, readErr)
			}
		}
		for path, before := range identities {
			after, statErr := os.Lstat(path)
			if statErr != nil || !os.SameFile(before, after) {
				t.Errorf("%s changed identity at %s: before=%v after=%v err=%v", attempt, path, before, after, statErr)
			}
		}
		var currentPhase string
		if queryErr := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&currentPhase); queryErr != nil || currentPhase != RewindBlocked {
			t.Errorf("%s journal phase=%q err=%v; want blocked", attempt, currentPhase, queryErr)
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
