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

func TestReconcileRewindInterruptedBeforeSwap(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	_, digest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: digest, TargetDigest: "target", StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileRewinds(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UnfinishedRewindFor(ctx, "cand"); ok {
		t.Fatal("pre-swap interruption not finalized")
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("staging directory not cleaned")
	}
	rec, err := s.GetCandidate(ctx, "cand")
	if err != nil || rec.Candidate.CandidateDigest != digest {
		t.Fatalf("candidate changed by pre-swap recovery: %+v", rec)
	}
}

func TestReconcileRewindCompletesSwap(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root, oldDigest := rewindCandidate(t, s, ctx, map[string]string{"a.txt": "one"})
	// Simulate the completed exchange: the candidate now holds the snapshot.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	j := RewindJournal{ID: "rw-1", CandidateID: "cand", SnapshotID: "snap-1", ExpectedDigest: oldDigest, TargetDigest: newDigest, StagingDir: staging}
	if err := s.BeginRewind(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRewindPhase(ctx, "rw-1", RewindPrepared, RewindSwapped, ""); err != nil {
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
