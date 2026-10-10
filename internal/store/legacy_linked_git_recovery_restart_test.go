package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
)

// A legacy transaction must not follow a linked-worktree .git pointer into
// the formal Git common directory after a process/database restart.
func TestLegacyLinkedGitTransactionsBlockAfterDatabaseRestart(t *testing.T) {
	t.Run("acceptance", func(t *testing.T) {
		ctx := context.Background()
		s, dbPath := newGoalStore(t)
		root := t.TempDir()
		formal := filepath.Join(root, "formal")
		incoming := filepath.Join(root, "incoming")
		commonDir := filepath.Join(root, "git-common")
		for _, dir := range []string{formal, incoming, filepath.Join(commonDir, "worktrees", "formal")} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(incoming, "board.txt"), []byte("candidate bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		pointer := []byte("gitdir: " + filepath.Join(commonDir, "worktrees", "formal") + "\n")
		if err := os.WriteFile(filepath.Join(formal, ".git"), pointer, 0600); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(commonDir, "config")
		if err := os.WriteFile(sentinel, []byte("common-dir sentinel: keep"), 0600); err != nil {
			t.Fatal(err)
		}
		sentinelInfo, err := os.Lstat(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		_, oldDigest, err := candidate.BuildManifestForPolicy(formal, candidate.ManifestPolicyLegacy)
		if err != nil {
			t.Fatal(err)
		}
		_, newDigest, err := candidate.BuildManifestForPolicy(incoming, candidate.ManifestPolicyLegacy)
		if err != nil {
			t.Fatal(err)
		}
		c := candidate.Candidate{ID: "legacy-linked-accept", ManifestPolicy: candidate.ManifestPolicyLegacy, FormalRoot: formal, CandidateRoot: incoming, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
		if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "legacy-linked-action", GoalID: "goal-1"}); err != nil {
			t.Fatal(err)
		}
		d := candidate.AcceptanceDecision{ID: "legacy-linked-accept-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
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
			t.Fatal("legacy acceptance with a linked Git pointer was not blocked after restart")
		}
		var phase string
		if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
			t.Fatalf("acceptance phase=%q err=%v; want blocked", phase, err)
		}
		if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
			t.Fatalf("blocked acceptance receipt exists=%t err=%v", ok, err)
		}
		assertLinkedCommonDirSentinelUnchanged(t, sentinel, sentinelInfo)
		if got, err := os.ReadFile(filepath.Join(formal, ".git")); err != nil || string(got) != string(pointer) {
			t.Fatalf("formal linked-worktree pointer=%q err=%v", got, err)
		}
		for path, want := range map[string]string{filepath.Join(formal, "board.txt"): "formal bytes", filepath.Join(incoming, "board.txt"): "candidate bytes"} {
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("project bytes at %s=%q want %q err=%v", path, got, want, err)
			}
		}
	})

	t.Run("rewind", func(t *testing.T) {
		ctx := context.Background()
		s, dbPath := newGoalStore(t)
		root := t.TempDir()
		current := filepath.Join(root, "candidate")
		staging := filepath.Join(root, "staging")
		commonDir := filepath.Join(root, "git-common")
		for _, dir := range []string{current, staging, filepath.Join(commonDir, "worktrees", "candidate")} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(current, "board.txt"), []byte("candidate bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, "board.txt"), []byte("snapshot bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		pointer := []byte("gitdir: " + filepath.Join(commonDir, "worktrees", "candidate") + "\n")
		if err := os.WriteFile(filepath.Join(current, ".git"), pointer, 0600); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(commonDir, "config")
		if err := os.WriteFile(sentinel, []byte("common-dir sentinel: keep"), 0600); err != nil {
			t.Fatal(err)
		}
		sentinelInfo, err := os.Lstat(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		_, currentDigest, err := candidate.BuildManifestForPolicy(current, candidate.ManifestPolicyLegacy)
		if err != nil {
			t.Fatal(err)
		}
		_, targetDigest, err := candidate.BuildManifestForPolicy(staging, candidate.ManifestPolicyLegacy)
		if err != nil {
			t.Fatal(err)
		}
		c := candidate.Candidate{ID: "legacy-linked-rewind", ManifestPolicy: candidate.ManifestPolicyLegacy, FormalRoot: filepath.Join(root, "formal"), CandidateRoot: current, BaselineDigest: currentDigest, CandidateDigest: currentDigest, Status: "ready"}
		if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "legacy-linked-rewind-action", GoalID: "goal-1"}); err != nil {
			t.Fatal(err)
		}
		j := RewindJournal{ID: "legacy-linked-rewind-journal", CandidateID: c.ID, SnapshotID: "snapshot", ExpectedDigest: currentDigest, TargetDigest: targetDigest, StagingDir: staging, ManifestPolicy: candidate.ManifestPolicyLegacy}
		if err := s.BeginRewind(ctx, j); err != nil {
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

		if err := s.ReconcileRewinds(ctx); err == nil {
			t.Fatal("legacy rewind with a linked Git pointer was not blocked after restart")
		}
		var phase string
		if err := s.DB().QueryRow(`SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&phase); err != nil || phase != RewindBlocked {
			t.Fatalf("rewind phase=%q err=%v; want blocked", phase, err)
		}
		assertLinkedCommonDirSentinelUnchanged(t, sentinel, sentinelInfo)
		if got, err := os.ReadFile(filepath.Join(current, ".git")); err != nil || string(got) != string(pointer) {
			t.Fatalf("candidate linked-worktree pointer=%q err=%v", got, err)
		}
		for path, want := range map[string]string{filepath.Join(current, "board.txt"): "candidate bytes", filepath.Join(staging, "board.txt"): "snapshot bytes"} {
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("rewind bytes at %s=%q want %q err=%v", path, got, want, err)
			}
		}
		record, err := s.GetCandidate(ctx, c.ID)
		if err != nil || record.Candidate.Status != "ready" || record.Candidate.CandidateDigest != currentDigest {
			t.Fatalf("blocked rewind changed candidate=%+v err=%v", record.Candidate, err)
		}
		if _, ok, err := s.FindAcceptanceReceipt(ctx, j.ID); err != nil || ok {
			t.Fatalf("rewind created an acceptance receipt=%t err=%v", ok, err)
		}
	})
}

func assertLinkedCommonDirSentinelUnchanged(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "common-dir sentinel: keep" {
		t.Fatalf("linked common-dir sentinel=%q err=%v", got, err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("linked common-dir sentinel identity changed: before=%v after=%v err=%v", before, after, err)
	}
}
