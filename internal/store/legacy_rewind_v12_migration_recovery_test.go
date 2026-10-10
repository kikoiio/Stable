package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A pending legacy rewind from a v12 database must retain its legacy policy
// across migration. Missing v14 root identities are unknown, so recovery must
// block instead of interpreting the old intent under the project-v2 rules.
func TestLegacyRewindV12MigrationBlocksWithoutInventingIdentity(t *testing.T) {
	ctx := context.Background()
	s, dbPath := newGoalStore(t)
	parent := t.TempDir()
	current := filepath.Join(parent, "candidate")
	staging := filepath.Join(parent, "staging")
	commonDir := filepath.Join(parent, "git-common")
	for _, dir := range []string{current, staging, filepath.Join(commonDir, "worktrees", "candidate")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		filepath.Join(current, "board.txt"): "current candidate bytes",
		filepath.Join(staging, "board.txt"): "rewind snapshot bytes",
	} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pointer := []byte("gitdir: " + filepath.Join(commonDir, "worktrees", "candidate") + "\n")
	if err := os.WriteFile(filepath.Join(current, ".git"), pointer, 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(commonDir, "config")
	if err := os.WriteFile(sentinel, []byte("common Git config sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	identities := make(map[string]os.FileInfo)
	for _, path := range []string{current, staging, filepath.Join(current, ".git"), sentinel} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		identities[path] = info
	}
	_, currentDigest, err := candidate.BuildManifestForPolicy(current, candidate.ManifestPolicyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{
		ID: "legacy-v12-rewind", ManifestPolicy: candidate.ManifestPolicyLegacy,
		FormalRoot: filepath.Join(parent, "formal"), CandidateRoot: current,
		BaselineDigest: currentDigest, CandidateDigest: currentDigest, Status: "ready",
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "legacy-v12-rewind-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{
		ID: "legacy-v12-rewind-journal", CandidateID: c.ID, SnapshotID: "legacy-snapshot",
		ExpectedDigest: currentDigest, TargetDigest: targetDigest, StagingDir: staging,
		ManifestPolicy: candidate.ManifestPolicyLegacy,
	}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`
		ALTER TABLE candidates DROP COLUMN manifest_policy;
		ALTER TABLE rewind_journal DROP COLUMN manifest_policy;
		ALTER TABLE rewind_journal DROP COLUMN expected_root_identity;
		ALTER TABLE rewind_journal DROP COLUMN target_root_identity;
		PRAGMA user_version=12;
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
	if candidatePolicy != candidate.ManifestPolicyLegacy || journalPolicy != candidate.ManifestPolicyLegacy || expectedIdentity != "" || targetIdentity != "" || phase != RewindPrepared {
		t.Fatalf("migrated legacy rewind changed contract: candidate policy=%q journal policy=%q identities=%q/%q phase=%q", candidatePolicy, journalPolicy, expectedIdentity, targetIdentity, phase)
	}

	if err := s.ReconcileRewinds(ctx); err == nil {
		t.Fatal("v12 legacy rewind with an untrusted linked Git pointer was not blocked")
	}
	assertPreserved := func(attempt string) {
		t.Helper()
		for path, want := range map[string]string{
			filepath.Join(current, "board.txt"): "current candidate bytes",
			filepath.Join(current, ".git"):      string(pointer),
			filepath.Join(staging, "board.txt"): "rewind snapshot bytes",
			sentinel:                            "common Git config sentinel",
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
			t.Errorf("%s rewind phase=%q err=%v; want blocked", attempt, currentPhase, queryErr)
		}
		record, getErr := s.GetCandidate(ctx, c.ID)
		if getErr != nil || record.Candidate.Status != "ready" || record.Candidate.CandidateDigest != currentDigest {
			t.Errorf("%s candidate changed: %+v err=%v", attempt, record.Candidate, getErr)
		}
	}
	assertPreserved("first recovery")
	if err := s.ReconcileRewinds(ctx); err != nil {
		t.Fatalf("repeat blocked rewind recovery: %v", err)
	}
	assertPreserved("repeat recovery")
}
