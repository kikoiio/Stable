package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"stable/internal/candidate"
	"stable/internal/platform/secfile"
)

// Rewind journal phases. A rewind commits in one atomic directory exchange;
// the journal exists so a crash anywhere around that exchange can be
// reconciled against real digests instead of guessing.
const (
	RewindPrepared        = "prepared"
	RewindOldSaved        = "old_saved"
	RewindTargetInstalled = "target_installed"
	RewindSwapped         = "swapped"
	RewindFinalized       = "finalized"
	RewindBlocked         = "blocked"
)

// RewindJournal tracks one candidate rewind attempt.
type RewindJournal struct {
	ExpectedRootIdentity string
	TargetRootIdentity   string
	ID                   string
	CandidateID          string
	SnapshotID           string
	Phase                string
	ExpectedDigest       string // candidate digest before the rewind
	TargetDigest         string // snapshot digest to restore
	StagingDir           string
	TransactionMode      string
	RollbackPath         string
	ManifestPolicy       string
	Reason               string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// BeginRewind records a prepared rewind. Only one unfinished rewind may
// exist per candidate.
func (s *Store) BeginRewind(ctx context.Context, j RewindJournal) error {
	if j.ID == "" || j.CandidateID == "" || j.SnapshotID == "" || j.ExpectedDigest == "" || j.TargetDigest == "" || j.StagingDir == "" {
		return errors.New("rewind journal attribution is incomplete")
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now().UTC()
	}
	if j.UpdatedAt.IsZero() {
		j.UpdatedAt = j.CreatedAt
	}
	if j.TransactionMode == "" {
		j.TransactionMode = secfile.TransactionMode()
	}
	if j.RollbackPath == "" {
		j.RollbackPath = j.StagingDir + ".rollback"
	}
	if j.ManifestPolicy == "" {
		if err := s.db.QueryRowContext(ctx, `SELECT manifest_policy FROM candidates WHERE id=?`, j.CandidateID).Scan(&j.ManifestPolicy); err != nil {
			return err
		}
	}
	var root string
	if err := s.db.QueryRowContext(ctx, `SELECT candidate_root FROM candidates WHERE id=?`, j.CandidateID).Scan(&root); err != nil {
		return err
	}
	expected, err := candidate.CaptureRootIdentity(root)
	if err != nil {
		return err
	}
	target, err := candidate.CaptureRootIdentity(j.StagingDir)
	if err != nil {
		return err
	}
	if j.ExpectedRootIdentity != "" && j.ExpectedRootIdentity != expected || j.TargetRootIdentity != "" && j.TargetRootIdentity != target {
		return errors.New("rewind root identity changed")
	}
	j.ExpectedRootIdentity, j.TargetRootIdentity = expected, target
	_, err = s.db.ExecContext(ctx, `INSERT INTO rewind_journal(id,candidate_id,snapshot_id,phase,transaction_mode,rollback_path,manifest_policy,expected_root_identity,target_root_identity,expected_digest,target_digest,staging_dir,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.CandidateID, j.SnapshotID, RewindPrepared, j.TransactionMode, j.RollbackPath, j.ManifestPolicy, j.ExpectedRootIdentity, j.TargetRootIdentity, j.ExpectedDigest, j.TargetDigest, j.StagingDir, "", j.CreatedAt.UTC().Format(time.RFC3339Nano), j.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// SetRewindPhase advances a rewind journal through its state machine.
func (s *Store) SetRewindPhase(ctx context.Context, id, from, to, reason string) error {
	if !validRewindTransition(from, to) {
		return fmt.Errorf("invalid rewind phase %s -> %s", from, to)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE rewind_journal SET phase=?,reason=?,updated_at=? WHERE id=? AND phase=?`, to, reason, time.Now().UTC().Format(time.RFC3339Nano), id, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("rewind phase changed")
	}
	return nil
}

func validRewindTransition(from, to string) bool {
	switch from + "->" + to {
	// prepared->finalized marks a crash that happened before the exchange:
	// the candidate still matches the expected digest, nothing changed.
	case "prepared->old_saved", "prepared->swapped", "prepared->finalized", "prepared->blocked", "old_saved->target_installed", "old_saved->blocked", "target_installed->swapped", "target_installed->blocked", "swapped->finalized", "swapped->blocked":
		return true
	}
	return false
}

// UnfinishedRewinds lists journals needing recovery, oldest first.
func (s *Store) UnfinishedRewinds(ctx context.Context) ([]RewindJournal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,candidate_id,snapshot_id,phase,expected_digest,target_digest,staging_dir,reason,created_at,updated_at,transaction_mode,rollback_path,manifest_policy,expected_root_identity,target_root_identity FROM rewind_journal WHERE phase IN ('prepared','old_saved','target_installed','swapped') ORDER BY updated_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RewindJournal
	for rows.Next() {
		j, err := scanRewind(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UnfinishedRewindFor returns the candidate's open rewind, if any. Accept
// and new rewind requests must refuse while one exists.
func (s *Store) UnfinishedRewindFor(ctx context.Context, candidateID string) (RewindJournal, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,candidate_id,snapshot_id,phase,expected_digest,target_digest,staging_dir,reason,created_at,updated_at,transaction_mode,rollback_path,manifest_policy,expected_root_identity,target_root_identity FROM rewind_journal WHERE candidate_id=? AND phase IN ('prepared','old_saved','target_installed','swapped') ORDER BY updated_at DESC LIMIT 1`, candidateID)
	if err != nil {
		return RewindJournal{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return RewindJournal{}, false, rows.Err()
	}
	j, err := scanRewind(rows)
	return j, true, err
}

type rewindScanner interface {
	Scan(dest ...any) error
}

func scanRewind(rows rewindScanner) (RewindJournal, error) {
	var j RewindJournal
	var created, updated string
	err := rows.Scan(&j.ID, &j.CandidateID, &j.SnapshotID, &j.Phase, &j.ExpectedDigest, &j.TargetDigest, &j.StagingDir, &j.Reason, &created, &updated, &j.TransactionMode, &j.RollbackPath, &j.ManifestPolicy, &j.ExpectedRootIdentity, &j.TargetRootIdentity)
	if err != nil {
		return j, err
	}
	if j.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return j, err
	}
	if j.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return j, err
	}
	return j, nil
}

// FinalizeRewind marks the swap complete and updates the candidate digest
// in one transaction. The candidate must still be ready and at the
// expected digest; anything else means another writer raced the rewind.
func (s *Store) FinalizeRewind(ctx context.Context, j RewindJournal) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE candidates SET candidate_digest=?,updated_at=? WHERE id=? AND status='ready' AND candidate_digest=?`, j.TargetDigest, time.Now().UTC().Format(time.RFC3339Nano), j.CandidateID, j.ExpectedDigest)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("candidate state changed during rewind")
	}
	res, err = tx.ExecContext(ctx, `UPDATE rewind_journal SET phase=?,reason='',updated_at=? WHERE id=? AND phase IN ('prepared','swapped')`, RewindFinalized, time.Now().UTC().Format(time.RFC3339Nano), j.ID)
	if err != nil {
		return err
	}
	if n, err = res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errors.New("rewind phase changed during finalize")
	}
	return tx.Commit()
}

// ReconcileRewinds finishes or blocks every unfinished journal after a
// restart by comparing on-disk digests with the journal, the same way
// ReconcileAcceptances settles interrupted accepts.
func (s *Store) ReconcileRewinds(ctx context.Context) error {
	items, err := s.UnfinishedRewinds(ctx)
	if err != nil {
		return err
	}
	for _, j := range items {
		if err = s.reconcileRewind(ctx, j); err != nil {
			return err
		}
	}
	// FinalizeRewind commits the database before Cleanup removes the spent
	// staging root. If the process stops between those operations, the journal
	// is no longer unfinished, so recover cleanup from finalized
	// journals whose candidate digest proves that the rewind committed.
	rows, err := s.db.QueryContext(ctx, `SELECT id,candidate_id,snapshot_id,phase,expected_digest,target_digest,staging_dir,reason,created_at,updated_at,transaction_mode,rollback_path,manifest_policy,expected_root_identity,target_root_identity FROM rewind_journal WHERE phase='finalized' AND expected_root_identity<>'' AND target_root_identity<>'' ORDER BY updated_at,id`)
	if err != nil {
		return err
	}
	var finalized []RewindJournal
	for rows.Next() {
		j, scanErr := scanRewind(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		finalized = append(finalized, j)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, j := range finalized {
		rec, getErr := s.GetCandidate(ctx, j.CandidateID)
		if getErr != nil {
			return getErr
		}
		if rec.Candidate.Status != "ready" || rec.Candidate.CandidateDigest != j.TargetDigest {
			continue
		}
		policy := j.ManifestPolicy
		if policy == "" {
			policy = candidate.ManifestPolicyLegacy
		}
		if rec.Candidate.ManifestPolicy != policy {
			return errors.New("finalized rewind candidate and journal manifest policies differ")
		}
		tx := candidate.DirectoryTransaction{
			ID: j.ID, Kind: candidate.TransactionRewind, ManifestPolicy: policy,
			ExpectedRootIdentity: j.ExpectedRootIdentity, TargetRootIdentity: j.TargetRootIdentity,
			CurrentRoot: rec.Candidate.CandidateRoot, IncomingRoot: j.StagingDir, RollbackRoot: j.RollbackPath,
			ExpectedDigest: j.ExpectedDigest, TargetDigest: j.TargetDigest, Mode: j.TransactionMode,
		}
		if err = candidate.NewTransactionCoordinator().Cleanup(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reconcileRewind(ctx context.Context, j RewindJournal) error {
	rec, err := s.GetCandidate(ctx, j.CandidateID)
	if err != nil {
		return s.blockRewind(ctx, j, fmt.Errorf("candidate unavailable: %w", err))
	}
	if rec.Candidate.FormalRoot == rec.Candidate.CandidateRoot {
		return s.blockRewind(ctx, j, errors.New("rewind target is the formal project"))
	}
	if j.ManifestPolicy == candidate.ManifestPolicyLegacy || j.ManifestPolicy == "" {
		if j.ExpectedRootIdentity == "" || j.TargetRootIdentity == "" {
			return s.blockRewind(ctx, j, errors.New("legacy rewind root identities are missing; retain paths for explicit reconciliation"))
		}
		if err := candidate.RejectLegacyProtectedMetadata(rec.Candidate.CandidateRoot, j.StagingDir, j.RollbackPath); err != nil {
			return s.blockRewind(ctx, j, err)
		}
	}
	if j.ExpectedRootIdentity == "" || j.TargetRootIdentity == "" {
		return s.blockRewind(ctx, j, errors.New("rewind root identities are missing; retain paths for explicit reconciliation"))
	}
	if j.Phase == RewindPrepared {
		discarded, err := s.discardEmptyPreparedRewind(ctx, j, rec.Candidate)
		if err != nil {
			return s.blockRewind(ctx, j, err)
		}
		if discarded {
			return nil
		}
	}
	tx := candidate.DirectoryTransaction{ID: j.ID, Kind: candidate.TransactionRewind, ManifestPolicy: j.ManifestPolicy, ExpectedRootIdentity: j.ExpectedRootIdentity, TargetRootIdentity: j.TargetRootIdentity, CurrentRoot: rec.Candidate.CandidateRoot, IncomingRoot: j.StagingDir, RollbackRoot: j.RollbackPath, ExpectedDigest: j.ExpectedDigest, TargetDigest: j.TargetDigest, Mode: j.TransactionMode}
	coordinator := candidate.NewTransactionCoordinator()
	state, err := coordinator.Inspect(ctx, tx, candidate.TransactionPhase(j.Phase))
	if err != nil {
		return s.blockRewind(ctx, j, err)
	}
	if state == candidate.RecoveryOld && j.Phase == RewindPrepared {
		err = coordinator.Apply(ctx, tx, rewindJournalAdapter{store: s})
	} else {
		err = coordinator.Recover(ctx, tx, candidate.TransactionPhase(j.Phase), rewindJournalAdapter{store: s})
	}
	if err != nil {
		return s.blockRewind(ctx, j, err)
	}
	if rec.Candidate.Status != "ready" {
		return s.blockRewind(ctx, j, fmt.Errorf("candidate status %q cannot finalize a rewind", rec.Candidate.Status))
	}
	if err = s.FinalizeRewind(ctx, j); err != nil {
		return s.blockRewind(ctx, j, err)
	}
	return coordinator.Cleanup(ctx, tx)
}

// discardEmptyPreparedRewind handles the crash cut where the journal and an
// empty staging directory were persisted before any snapshot bytes were
// materialized. Empty owned staging is safe to discard, but incomplete or
// unknown contents are retained for explicit reconciliation.
func (s *Store) discardEmptyPreparedRewind(ctx context.Context, j RewindJournal, c candidate.Candidate) (bool, error) {
	if c.Status != "ready" {
		return false, nil
	}
	currentIdentity, err := candidate.CaptureRootIdentity(c.CandidateRoot)
	if err != nil || currentIdentity != j.ExpectedRootIdentity {
		return false, nil
	}
	_, currentDigest, err := candidate.BuildManifestForPolicy(c.CandidateRoot, j.ManifestPolicy)
	if err != nil || currentDigest != j.ExpectedDigest {
		return false, nil
	}
	if _, err := os.Lstat(j.RollbackPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	targetIdentity, err := candidate.CaptureRootIdentity(j.StagingDir)
	if os.IsNotExist(err) {
		return true, s.SetRewindPhase(ctx, j.ID, RewindPrepared, RewindFinalized, "interrupted before snapshot materialization; staging already absent")
	}
	if err != nil || targetIdentity != j.TargetRootIdentity {
		return false, nil
	}
	entries, err := os.ReadDir(j.StagingDir)
	if err != nil {
		return false, err
	}
	if len(entries) != 0 {
		return false, nil
	}
	if err := os.Remove(j.StagingDir); err != nil {
		return false, err
	}
	return true, s.SetRewindPhase(ctx, j.ID, RewindPrepared, RewindFinalized, "interrupted before snapshot materialization; empty staging removed")
}

type rewindJournalAdapter struct{ store *Store }

func (j rewindJournalAdapter) Advance(ctx context.Context, id string, from, to candidate.TransactionPhase, reason string) error {
	return j.store.SetRewindPhase(ctx, id, string(from), string(to), reason)
}

func (s *Store) blockRewind(ctx context.Context, j RewindJournal, cause error) error {
	var currentPhase string
	if err := s.db.QueryRowContext(ctx, `SELECT phase FROM rewind_journal WHERE id=?`, j.ID).Scan(&currentPhase); err != nil {
		return err
	}
	if currentPhase != RewindBlocked {
		if err := s.SetRewindPhase(ctx, j.ID, currentPhase, RewindBlocked, cause.Error()); err != nil {
			return err
		}
	}
	return fmt.Errorf("rewind %s blocked during recovery: %w", j.ID, cause)
}
