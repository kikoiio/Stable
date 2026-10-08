package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/core"
)

func TestCandidateTransitionAndReviewPersistence(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	r := CandidateRecord{Candidate: candidate.Candidate{ID: "cand", FormalRoot: "/formal", CandidateRoot: "/candidate", BaselineDigest: "base", Status: "prepared"}, ActionID: "action", GoalID: "goal-1"}
	if err := s.SaveCandidate(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "prepared", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "running", "ready", "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidate(ctx, "cand", "ready", "accepted", ""); err == nil {
		t.Fatal("invalid transition accepted")
	}
	loaded, err := s.GetCandidate(ctx, "cand")
	if err != nil || loaded.Candidate.CandidateDigest != "candidate" || loaded.Candidate.Status != "ready" {
		t.Fatalf("candidate=%+v err=%v", loaded, err)
	}
	review := candidate.Review{ID: "preview", CandidateID: "cand", FormalDigest: "base", CandidateDigest: "candidate", Digest: "preview-digest", Changes: []candidate.FileChange{{Path: "file", Status: "modified"}}}
	if err = s.SaveCandidateReview(ctx, review); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetCandidateReview(ctx, review.ID)
	if err != nil || again.Digest != review.Digest || len(again.Changes) != 1 {
		t.Fatalf("review=%+v err=%v", again, err)
	}
}

func TestAcceptanceFinalizeIsIdempotentAndQueuesReverification(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c := candidate.Candidate{ID: "cand", FormalRoot: "/formal", CandidateRoot: "/candidate", BaselineDigest: "old", CandidateDigest: "new", Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "decision", CandidateID: "cand", UserID: "local-user", CandidateDigest: "new", PreviewDigest: "preview", FormalDigest: "old", Mode: candidate.AcceptNormal}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAcceptancePhase(ctx, d.ID, "prepared", "swapped", ""); err != nil {
		t.Fatal(err)
	}
	receipt := candidate.Receipt{ID: "receipt", DecisionID: d.ID, CandidateID: c.ID, FormalDigest: "new", AcceptedAt: now}
	if err := s.FinalizeAcceptance(ctx, d, receipt, "goal-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeAcceptance(ctx, d, receipt, "goal-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAcceptanceReceipt(ctx, d.ID)
	if err != nil || got.ID != receipt.ID {
		t.Fatalf("receipt=%+v err=%v", got, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.CurrentArtifactID != "new" {
		t.Fatalf("goal not awaiting independent reverification: %+v", snapshot.Goal)
	}
	if len(snapshot.Events) != 1 || snapshot.Events[0].ID != "accept-decision" {
		t.Fatalf("acceptance wake events=%+v", snapshot.Events)
	}
}

func TestReconcilePreparedAcceptanceBeforeExchange(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(candRoot)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{ID: "reconcile", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "reconcile-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err = s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal after reconciliation=%q err=%v", data, err)
	}
	receipt, ok, err := s.FindAcceptanceReceipt(ctx, d.ID)
	if err != nil || !ok || receipt.FormalDigest != newDigest {
		t.Fatalf("receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	if err = s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
}

func TestReconcileAcceptanceAfterExchangeBeforeJournalUpdate(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600)
	_ = os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600)
	_, oldDigest, _ := candidate.BuildManifest(formal)
	_, newDigest, _ := candidate.BuildManifest(candRoot)
	c := candidate.Candidate{ID: "crash-after-exchange", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "crash-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptForce}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := candidate.ExchangeProjectDir(formal, candRoot); err != nil {
		t.Fatal(err)
	} // crash before phase becomes swapped
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "finalized" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
}

func TestReconcileAcceptanceConflictBlocks(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candRoot := filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600)
	_ = os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600)
	_, oldDigest, _ := candidate.BuildManifest(formal)
	_, newDigest, _ := candidate.BuildManifest(candRoot)
	c := candidate.Candidate{ID: "conflict", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err := s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "conflict-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(formal, "board"), []byte("external"), 0600)
	if err := s.ReconcileAcceptances(ctx); err == nil {
		t.Fatal("digest conflict did not block")
	}
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
}

func TestAcceptCandidateUsesDurableJournal(t *testing.T) {
	s, dbPath := newGoalStore(t)
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := candidate.CreateCandidate("durable", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = candidate.FreezeCandidate(c, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "reviewed"
	review, err := candidate.BuildReview(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "action", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d := candidate.AcceptanceDecision{ID: "durable-decision", UserID: "local-user", CandidateID: c.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: candidate.AcceptNormal}
	receipt, err := candidate.AcceptCandidate(ctx, c, review, d, "goal-1", "", s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	second, err := candidate.AcceptCandidate(ctx, c, review, d, "goal-1", "", s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != receipt.ID {
		t.Fatalf("retry changed receipt: %+v != %+v", second, receipt)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal project after acceptance=%q err=%v", data, err)
	}
}

func TestProjectMetadataAcceptanceRecoveryEveryMoveBoundary(t *testing.T) {
	cases := []struct {
		name, phase     string
		moves, restored int
	}{
		{"intent", "prepared", 0, 0}, {"old_move_before_journal", "prepared", 1, 0},
		{"old_saved", "old_saved", 1, 0}, {"target_move_before_journal", "old_saved", 2, 0},
		{"target_installed", "target_installed", 2, 0}, {"rollback_move_before_journal", "target_installed", 3, 0},
		{"swapped", "swapped", 3, 0}, {"git_restored", "swapped", 3, 1},
		{"service_restored", "swapped", 3, 2}, {"before_finalize", "swapped", 3, 3},
		{"metadata_destination_conflict", "swapped", 3, 0},
		{"same_digest_git_pointer_replacement", "swapped", 3, 0},
		{"same_digest_formal_replacement", "prepared", 0, 0},
		{"same_digest_candidate_replacement", "prepared", 0, 0},
		{"same_digest_rollback_replacement", "old_saved", 1, 0},
		{"legacy_v13_missing_root_identity", "prepared", 0, 0},
	}
	for _, gitDirectory := range []bool{false, true} {
		for _, fixture := range cases {
			if fixture.name == "same_digest_git_pointer_replacement" && gitDirectory {
				continue
			}
			t.Run(fmt.Sprintf("git_directory_%t/%s", gitDirectory, fixture.name), func(t *testing.T) {
				s, dbPath := newGoalStore(t)
				ctx := context.Background()
				parent := t.TempDir()
				formal, incoming, rollback := filepath.Join(parent, "formal"), filepath.Join(parent, "incoming"), filepath.Join(parent, "rollback")
				for _, root := range []string{formal, incoming} {
					if err := os.Mkdir(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
				for root, value := range map[string]string{formal: "old", incoming: "new"} {
					if err := os.WriteFile(filepath.Join(root, "board"), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				metadataFiles := map[string]string{".git": "gitdir: /unmounted/external/private\n", ".stable/session": "live log", ".mewcode/history": "legacy state"}
				if gitDirectory {
					delete(metadataFiles, ".git")
					metadataFiles[".git/config"] = "private git config"
				}
				for name, value := range metadataFiles {
					path := filepath.Join(formal, name)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				facts, err := candidate.CaptureProtectedMetadata(formal)
				if err != nil {
					t.Fatal(err)
				}
				_, oldDigest, err := candidate.BuildManifestForPolicy(formal, candidate.ManifestPolicyProject)
				if err != nil {
					t.Fatal(err)
				}
				_, newDigest, err := candidate.BuildManifestForPolicy(incoming, candidate.ManifestPolicyProject)
				if err != nil {
					t.Fatal(err)
				}
				c := candidate.Candidate{ID: "v2", ManifestPolicy: candidate.ManifestPolicyProject, FormalRoot: formal, CandidateRoot: incoming, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
				if err = s.SaveCandidate(ctx, CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
					t.Fatal(err)
				}
				d := candidate.AcceptanceDecision{ID: "v2-decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
				if _, err = s.SaveAcceptanceDecision(ctx, d); err != nil {
					t.Fatal(err)
				}
				if err = s.SaveProtectedMetadata(ctx, d.ID, facts); err != nil {
					t.Fatal(err)
				}
				if _, err = s.DB().Exec(`UPDATE acceptance_apply_journal SET transaction_mode='journaled-move',rollback_path=?,phase=? WHERE decision_id=?`, rollback, fixture.phase, d.ID); err != nil {
					t.Fatal(err)
				}
				moves := [][2]string{{formal, rollback}, {incoming, formal}, {rollback, incoming}}
				for _, move := range moves[:fixture.moves] {
					if err = os.Rename(move[0], move[1]); err != nil {
						t.Fatal(err)
					}
				}
				for _, fact := range facts[:fixture.restored] {
					if err = os.Rename(filepath.Join(incoming, fact.Name), filepath.Join(formal, fact.Name)); err != nil {
						t.Fatal(err)
					}
				}
				replacedRoot := ""
				switch fixture.name {
				case "same_digest_formal_replacement":
					replacedRoot = formal
				case "same_digest_candidate_replacement":
					replacedRoot = incoming
				case "same_digest_rollback_replacement":
					replacedRoot = rollback
				}
				if replacedRoot != "" {
					original := replacedRoot + "-external-original"
					if err := os.Rename(replacedRoot, original); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(replacedRoot, 0700); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(filepath.Join(original, "board"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(replacedRoot, "board"), data, 0600); err != nil {
						t.Fatal(err)
					}
					// Keep even the original metadata entities: the data root's
					// persisted identity must independently reject replacement.
					for _, fact := range facts {
						if _, err := os.Lstat(filepath.Join(original, fact.Name)); err == nil {
							if err := os.Rename(filepath.Join(original, fact.Name), filepath.Join(replacedRoot, fact.Name)); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				if fixture.name == "legacy_v13_missing_root_identity" {
					if _, err := s.DB().Exec(`ALTER TABLE acceptance_apply_journal DROP COLUMN expected_root_identity; ALTER TABLE acceptance_apply_journal DROP COLUMN target_root_identity; PRAGMA user_version=13`); err != nil {
						t.Fatal(err)
					}
				}
				if fixture.name == "metadata_destination_conflict" {
					if err := os.WriteFile(filepath.Join(formal, ".git"), []byte("external metadata"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if fixture.name == "same_digest_git_pointer_replacement" {
					formalGit := filepath.Join(formal, ".git")
					incomingGit := filepath.Join(incoming, ".git")
					if _, err := os.Lstat(formalGit); !os.IsNotExist(err) {
						t.Fatalf("formal .git exists before metadata restore: %v", err)
					}
					if data, err := os.ReadFile(filepath.Join(formal, "board")); err != nil || string(data) != "new" {
						t.Fatalf("formal content before recovery=%q err=%v", data, err)
					}
					originalInfo, err := os.Lstat(incomingGit)
					if err != nil || !originalInfo.Mode().IsRegular() {
						t.Fatalf("incoming .git pointer info=%v err=%v", originalInfo, err)
					}
					pointer, err := os.ReadFile(incomingGit)
					if err != nil {
						t.Fatal(err)
					}
					replacement := filepath.Join(parent, "replacement.git")
					if err = os.WriteFile(replacement, pointer, 0600); err != nil {
						t.Fatal(err)
					}
					replacementInfo, err := os.Lstat(replacement)
					if err != nil || os.SameFile(originalInfo, replacementInfo) {
						t.Fatalf(".git pointer was not replaced: original=%v replacement=%v err=%v", originalInfo, replacementInfo, err)
					}
					if err = os.Rename(replacement, incomingGit); err != nil {
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
				if replacedRoot != "" || fixture.name == "legacy_v13_missing_root_identity" {
					if err := s.ReconcileAcceptances(ctx); err == nil {
						t.Fatal("unknown root identity was accepted")
					}
					var phase string
					if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
						t.Fatalf("phase=%s err=%v", phase, err)
					}
					if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
						t.Fatalf("blocked acceptance produced receipt: %t %v", ok, err)
					}
					if replacedRoot != "" {
						for _, path := range []string{replacedRoot, replacedRoot + "-external-original"} {
							if _, err := os.Lstat(path); err != nil {
								t.Fatalf("unknown resource deleted: %s %v", path, err)
							}
						}
					}
					var version int
					if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 14 {
						t.Fatalf("version=%d err=%v", version, err)
					}
					if err := s.ReconcileAcceptances(ctx); err != nil {
						t.Fatal(err)
					}
					return
				}
				if fixture.name == "metadata_destination_conflict" {
					if err := s.ReconcileAcceptances(ctx); err == nil {
						t.Fatal("concurrent metadata destination was accepted")
					}
					data, err := os.ReadFile(filepath.Join(formal, ".git"))
					if err != nil || string(data) != "external metadata" {
						t.Fatalf("destination changed: %q %v", data, err)
					}
					retained, err := candidate.CaptureProtectedMetadata(incoming)
					if err != nil {
						t.Fatal(err)
					}
					for i, fact := range retained {
						if fact != facts[i] {
							t.Fatalf("source metadata %s changed: %+v", fact.Name, fact)
						}
					}
					var phase string
					if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
						t.Fatalf("phase=%s err=%v", phase, err)
					}
					if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
						t.Fatalf("blocked acceptance produced receipt: %t %v", ok, err)
					}
					if err := s.ReconcileAcceptances(ctx); err != nil {
						t.Fatal(err)
					}
					return
				}
				if fixture.name == "same_digest_git_pointer_replacement" {
					if err := s.ReconcileAcceptances(ctx); err == nil {
						t.Fatal("recovery accepted a byte-identical replacement Git pointer")
					}
					data, err := os.ReadFile(filepath.Join(formal, "board"))
					if err != nil || string(data) != "new" {
						t.Fatalf("formal=%q err=%v", data, err)
					}
					data, err = os.ReadFile(filepath.Join(incoming, ".git"))
					if err != nil || string(data) != "gitdir: /unmounted/external/private\n" {
						t.Fatalf("replacement Git pointer=%q err=%v", data, err)
					}
					if _, err = os.Lstat(filepath.Join(formal, ".git")); !os.IsNotExist(err) {
						t.Fatalf("failed recovery changed formal metadata destination: %v", err)
					}
					var phase string
					if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
						t.Fatalf("phase=%s err=%v", phase, err)
					}
					if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
						t.Fatalf("blocked acceptance produced receipt: %t %v", ok, err)
					}
					if err := s.ReconcileAcceptances(ctx); err != nil {
						t.Fatalf("repeat blocked recovery: %v", err)
					}
					data, err = os.ReadFile(filepath.Join(formal, "board"))
					if err != nil || string(data) != "new" {
						t.Fatalf("repeat recovery changed formal content=%q err=%v", data, err)
					}
					data, err = os.ReadFile(filepath.Join(incoming, ".git"))
					if err != nil || string(data) != "gitdir: /unmounted/external/private\n" {
						t.Fatalf("repeat recovery changed replacement pointer=%q err=%v", data, err)
					}
					if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
						t.Fatalf("repeat recovery phase=%s err=%v", phase, err)
					}
					if _, ok, err := s.FindAcceptanceReceipt(ctx, d.ID); err != nil || ok {
						t.Fatalf("repeat blocked recovery produced receipt: %t %v", ok, err)
					}
					return
				}
				for attempt := 0; attempt < 2; attempt++ {
					if err = s.ReconcileAcceptances(ctx); err != nil {
						t.Fatalf("recovery %d: %v", attempt, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(formal, "board"))
				if err != nil || string(data) != "new" {
					t.Fatalf("formal=%q err=%v", data, err)
				}
				if err = candidate.ValidateProtectedMetadataFacts(formal, incoming, facts); err != nil {
					t.Fatal(err)
				}
				for name, value := range metadataFiles {
					data, err := os.ReadFile(filepath.Join(formal, name))
					if err != nil || string(data) != value {
						t.Fatalf("metadata %s=%q err=%v", name, data, err)
					}
				}
				var count int
				if err = s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("receipt count=%d err=%v", count, err)
				}
			})
		}
	}
}
