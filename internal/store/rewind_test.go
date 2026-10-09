package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/candidate"
)

func rewindCandidate(t *testing.T, s *Store, ctx context.Context, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, digest, err := candidate.BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{ID: "cand", FormalRoot: t.TempDir(), CandidateRoot: root, BaselineDigest: digest, CandidateDigest: digest, Status: "ready"}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	return root, digest
}

func TestRewindJournalLifecycle(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	staging := t.TempDir()
	c := candidate.Candidate{ID: "cand", FormalRoot: t.TempDir(), CandidateRoot: t.TempDir(), BaselineDigest: "old", Status: "ready"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginRewind(ctx, RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: "old", TargetDigest: "new", StagingDir: staging}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.UnfinishedRewindFor(ctx, "cand"); err != nil || !ok {
		t.Fatalf("unfinished = %t, %v", ok, err)
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindPrepared, RewindSwapped, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindSwapped, RewindPrepared, ""); err == nil {
		t.Fatal("backwards phase accepted")
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindSwapped, RewindFinalized, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.UnfinishedRewindFor(ctx, "cand"); err != nil || ok {
		t.Fatalf("finalized rewind still unfinished = %t, %v", ok, err)
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindFinalized, RewindBlocked, "late"); err == nil {
		t.Fatal("finalized rewind moved again")
	}
	if err := s.BeginRewind(ctx, RewindJournal{ID: "rw-2", CandidateID: "cand", SnapshotID: "snap-2", ExpectedDigest: "x", TargetDigest: "y", StagingDir: ""}); err == nil {
		t.Fatal("journal without staging dir accepted")
	}
}

func TestReconcileLegacyRewindCompletesPreparedTransaction(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, digest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "a.txt"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: digest, TargetDigest: targetDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UnfinishedRewindFor(ctx, "cand"); ok {
		t.Fatal("prepared rewind was not finalized")
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatalf("spent staging root not cleaned: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(got) != "snapshot" {
		t.Fatalf("candidate did not recover valid legacy target: %q err=%v", got, err)
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != targetDigest {
		t.Fatalf("candidate after valid legacy recovery: %+v err=%v", rec, err)
	}
}

func TestReconcileLegacyRewindRetainsReplacedStaging(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"board": "original"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-replaced-staging", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: targetDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	originalStaging := staging + ".original"
	if err := os.Rename(staging, originalStaging); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("unknown replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("replaced legacy staging was not blocked: %v", err)
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&phase); err != nil || phase != RewindBlocked {
		t.Fatalf("rewind phase=%q err=%v; want blocked", phase, err)
	}
	got, err := os.ReadFile(filepath.Join(staging, "board"))
	currentInfo, statErr := os.Stat(staging)
	if err != nil || string(got) != "unknown replacement" || statErr != nil || !os.SameFile(replacementInfo, currentInfo) {
		t.Fatalf("replacement staging changed: bytes=%q info=%v err=%v stat=%v", got, currentInfo, err, statErr)
	}
	original, err := os.ReadFile(filepath.Join(originalStaging, "board"))
	if err != nil || string(original) != "snapshot" {
		t.Fatalf("original staging was lost: %q err=%v", original, err)
	}
	board, err := os.ReadFile(filepath.Join(root, "board"))
	if err != nil || string(board) != "original" {
		t.Fatalf("candidate changed during blocked recovery: %q err=%v", board, err)
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != oldDigest || rec.Candidate.Status != "ready" {
		t.Fatalf("candidate record changed during blocked recovery: %+v err=%v", rec, err)
	}
}

func TestReconcileLegacyRewindRetainsUnknownPreparedStaging(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"board": "original"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-unknown-prepared-staging", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: targetDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "partial.bin"), []byte("unrecognized partial data"), 0600); err != nil {
		t.Fatal(err)
	}
	stagingInfo, err := os.Stat(staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("unrecognized prepared staging was not blocked: %v", err)
	}
	for path, want := range map[string]string{filepath.Join(root, "board"): "original", filepath.Join(staging, "board"): "snapshot", filepath.Join(staging, "partial.bin"): "unrecognized partial data"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("recovery changed %s: got=%q want=%q err=%v", path, got, want, err)
		}
	}
	currentInfo, err := os.Stat(staging)
	if err != nil || !os.SameFile(stagingInfo, currentInfo) {
		t.Fatalf("recovery changed staging root identity: before=%v after=%v err=%v", stagingInfo, currentInfo, err)
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != oldDigest || rec.Candidate.Status != "ready" {
		t.Fatalf("candidate changed during blocked recovery: %+v err=%v", rec, err)
	}
}

func TestReconcileLegacyRewindWithoutRecordedIdentityRetainsStaging(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"board": "original"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-missing-identity", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: targetDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE rewind_journal SET expected_root_identity='',target_root_identity='' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("legacy rewind without identities was not blocked: %v", err)
	}
	for path, want := range map[string]string{filepath.Join(root, "board"): "original", filepath.Join(staging, "board"): "snapshot"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("unidentified rewind changed %s: got=%q want=%q err=%v", path, got, want, err)
		}
	}
}

func TestReconcileRewindCompletesSwap(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "a.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: newDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash immediately after the atomic exchange but before its
	// journal phase advances.
	if err := candidate.SwapWithStaging(root, staging); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != newDigest || rec.Candidate.Status != "ready" {
		t.Fatalf("candidate after recovery: %+v, %v", rec, err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("staging directory not cleaned")
	}
}

func TestReconcileRewindBlocksOnMismatch(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: "unrelated", StagingDir: t.TempDir()}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("mismatch not blocked: %v", err)
	}
}

func TestReconcileRewindRefusesAcceptedCandidate(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "ready", "reviewed", ""); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: newDigest, StagingDir: t.TempDir()}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindPrepared, RewindSwapped, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil {
		t.Fatal("rewind finalized on a non-ready candidate")
	}
}

func TestRewindJournalMigration(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	c := candidate.Candidate{ID: "cand", FormalRoot: t.TempDir(), CandidateRoot: t.TempDir(), BaselineDigest: "a", Status: "ready"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginRewind(ctx, RewindJournal{ID: "rw-m", CandidateID: "cand", SnapshotID: "snap", ExpectedDigest: "a", TargetDigest: "b", StagingDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok, err := reopened.UnfinishedRewindFor(ctx, "cand"); err != nil || !ok {
		t.Fatalf("journal lost across reopen: %t, %v", ok, err)
	}
}

func TestJournaledRewindRecoveryEveryMoveBoundary(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		moves int
	}{
		{"old_move_before_journal", RewindPrepared, 1}, {"old_saved", RewindOldSaved, 1},
		{"target_move_before_journal", RewindOldSaved, 2}, {"target_installed", RewindTargetInstalled, 2},
		{"rollback_move_before_journal", RewindTargetInstalled, 3}, {"swapped", RewindSwapped, 3},
	}
	for _, policy := range []string{candidate.ManifestPolicyLegacy, candidate.ManifestPolicyProject} {
		for _, fixture := range cases {
			t.Run(policy+"/"+fixture.name, func(t *testing.T) {
				s, dbPath := newGoalStore(t)
				ctx := context.Background()
				root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"board": "old"})
				if _, err := s.DB().Exec(`UPDATE candidates SET manifest_policy=? WHERE id='cand'`, policy); err != nil {
					t.Fatal(err)
				}
				staging := filepath.Join(filepath.Dir(root), "staging")
				rollback := filepath.Join(filepath.Dir(root), "rollback")
				if err := os.Mkdir(staging, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(staging, "board"), []byte("new"), 0600); err != nil {
					t.Fatal(err)
				}
				_, newDigest, err := candidate.BuildManifest(staging)
				if err != nil {
					t.Fatal(err)
				}
				j := RewindJournal{ID: "rw", CandidateID: "cand", SnapshotID: "snap", ExpectedDigest: oldDigest, TargetDigest: newDigest, StagingDir: staging, RollbackPath: rollback, TransactionMode: "journaled-move"}
				if err = s.BeginRewind(ctx, j); err != nil {
					t.Fatal(err)
				}
				if _, err = s.DB().Exec(`UPDATE rewind_journal SET phase=? WHERE id=?`, fixture.phase, j.ID); err != nil {
					t.Fatal(err)
				}
				moves := [][2]string{{root, rollback}, {staging, root}, {rollback, staging}}
				for _, move := range moves[:fixture.moves] {
					if err = os.Rename(move[0], move[1]); err != nil {
						t.Fatal(err)
					}
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				for attempt := 0; attempt < 2; attempt++ {
					if err = s.ReconcileRewinds(ctx); err != nil {
						t.Fatalf("recovery %d: %v", attempt, err)
					}
				}
				rec, err := s.GetCandidate(ctx, "cand")
				if err != nil || rec.Candidate.CandidateDigest != newDigest {
					t.Fatalf("candidate=%+v err=%v", rec, err)
				}
				data, err := os.ReadFile(filepath.Join(root, "board"))
				if err != nil || string(data) != "new" {
					t.Fatalf("rewind=%q err=%v", data, err)
				}
				if _, err = os.Lstat(rollback); !os.IsNotExist(err) {
					t.Fatalf("rollback was stranded: %v", err)
				}
			})
		}
	}
}

func TestReconcileRewindBlocksAndRetainsUnknownProtectedMetadata(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"board": "old"})
	if _, err := s.DB().Exec(`UPDATE candidates SET manifest_policy=? WHERE id='cand'`, candidate.ManifestPolicyProject); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-unknown-metadata", CandidateID: "cand", SnapshotID: "snap", ExpectedDigest: oldDigest, TargetDigest: targetDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	unknownMetadata := filepath.Join(staging, ".mewcode", "private-state")
	if err := os.MkdirAll(filepath.Dir(unknownMetadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknownMetadata, []byte("preserve me"), 0600); err != nil {
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
		t.Fatal("rewind with unexpected protected metadata was accepted")
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&phase); err != nil || phase != RewindBlocked {
		t.Fatalf("phase=%q err=%v", phase, err)
	}
	for path, want := range map[string]string{filepath.Join(root, "board"): "old", filepath.Join(staging, "board"): "snapshot", unknownMetadata: "preserve me"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("preserved %s=%q want %q err=%v", path, got, want, err)
		}
	}
	if _, err := os.Lstat(j.RollbackPath); !os.IsNotExist(err) {
		t.Fatalf("unexpected rollback root was created: %v", err)
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != oldDigest || rec.Candidate.Status != "ready" {
		t.Fatalf("candidate changed by blocked recovery: %+v err=%v", rec, err)
	}
}

func TestLegacyRewindBlocksProtectedMewcode(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "board"), []byte("original board"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, ".mewcode", "history")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte("legacy protected metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataInfo, err := os.Stat(metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifestForPolicy(root, candidate.ManifestPolicyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: candidate.Candidate{
		ID: "legacy-mewcode-candidate", ManifestPolicy: candidate.ManifestPolicyLegacy,
		FormalRoot: t.TempDir(), CandidateRoot: root, BaselineDigest: oldDigest,
		CandidateDigest: oldDigest, Status: "ready",
	}, ActionID: "legacy-mewcode-action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "board"), []byte("snapshot board"), 0600); err != nil {
		t.Fatal(err)
	}
	_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{
		ID: "legacy-mewcode-rewind", CandidateID: "legacy-mewcode-candidate", SnapshotID: "legacy-mewcode-snapshot",
		ExpectedDigest: oldDigest, TargetDigest: targetDigest, StagingDir: staging,
	}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err == nil {
		t.Fatal("legacy rewind with protected .mewcode was accepted")
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&phase); err != nil || phase != RewindBlocked {
		t.Fatalf("rewind phase=%q err=%v; want blocked", phase, err)
	}
	if got, err := os.ReadFile(metadata); err != nil || string(got) != "legacy protected metadata" {
		t.Fatalf(".mewcode bytes changed: got=%q err=%v", got, err)
	}
	currentMetadataInfo, err := os.Stat(metadata)
	if err != nil || !os.SameFile(metadataInfo, currentMetadataInfo) {
		t.Fatalf(".mewcode inode changed: before=%v after=%v err=%v", metadataInfo, currentMetadataInfo, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "board")); err != nil || string(got) != "original board" {
		t.Fatalf("candidate root changed: board=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(staging, "board")); err != nil || string(got) != "snapshot board" {
		t.Fatalf("staging root changed: board=%q err=%v", got, err)
	}
	if rec, err := s.GetCandidate(ctx, j.CandidateID); err != nil || rec.Candidate.CandidateDigest != oldDigest || rec.Candidate.Status != "ready" {
		t.Fatalf("candidate changed after blocked rewind: %+v err=%v", rec, err)
	}
}
