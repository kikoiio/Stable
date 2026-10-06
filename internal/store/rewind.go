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
	ID              string
	CandidateID     string
	SnapshotID      string
	Phase           string
	ExpectedDigest  string // candidate digest before the rewind
	TargetDigest    string // snapshot digest to restore
	StagingDir      string
	TransactionMode string
	RollbackPath    string
	Reason          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO rewind_journal(id,candidate_id,snapshot_id,phase,transaction_mode,rollback_path,expected_digest,target_digest,staging_dir,reason,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.CandidateID, j.SnapshotID, RewindPrepared, j.TransactionMode, j.RollbackPath, j.ExpectedDigest, j.TargetDigest, j.StagingDir, "", j.CreatedAt.UTC().Format(time.RFC3339Nano), j.UpdatedAt.UTC().Format(time.RFC3339Nano))
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,candidate_id,snapshot_id,phase,expected_digest,target_digest,staging_dir,reason,created_at,updated_at,transaction_mode,rollback_path FROM rewind_journal WHERE phase IN ('prepared','old_saved','target_installed','swapped') ORDER BY updated_at,id`)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,candidate_id,snapshot_id,phase,expected_digest,target_digest,staging_dir,reason,created_at,updated_at,transaction_mode,rollback_path FROM rewind_journal WHERE candidate_id=? AND phase IN ('prepared','old_saved','target_installed','swapped') ORDER BY updated_at DESC LIMIT 1`, candidateID)
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
	err := rows.Scan(&j.ID, &j.CandidateID, &j.SnapshotID, &j.Phase, &j.ExpectedDigest, &j.TargetDigest, &j.StagingDir, &j.Reason, &created, &updated, &j.TransactionMode, &j.RollbackPath)
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
	_, digest, err := candidate.BuildManifest(rec.Candidate.CandidateRoot)
	if err != nil {
		return s.blockRewind(ctx, j, err)
	}
	if j.Phase == RewindOldSaved || j.Phase == RewindTargetInstalled {
		tx := candidate.DirectoryTransaction{ID: j.ID, Kind: candidate.TransactionRewind, CurrentRoot: rec.Candidate.CandidateRoot, IncomingRoot: j.StagingDir, RollbackRoot: j.RollbackPath, ExpectedDigest: j.ExpectedDigest, TargetDigest: j.TargetDigest, Mode: j.TransactionMode}
		if tx.Mode == "" {
			tx.Mode = "atomic-exchange"
		}
		if err = candidate.NewTransactionCoordinator().Recover(ctx, tx, candidate.TransactionPhase(j.Phase), rewindJournalAdapter{store: s}); err != nil {
			return s.blockRewind(ctx, j, err)
		}
		_, digest, err = candidate.BuildManifest(rec.Candidate.CandidateRoot)
		if err != nil {
			return s.blockRewind(ctx, j, err)
		}
	}
	switch digest {
	case j.ExpectedDigest:
		if j.Phase == RewindPrepared {
			if _, stagedDigest, stagedErr := candidate.BuildManifest(j.StagingDir); stagedErr == nil && stagedDigest == j.TargetDigest {
				tx := candidate.DirectoryTransaction{ID: j.ID, Kind: candidate.TransactionRewind, CurrentRoot: rec.Candidate.CandidateRoot, IncomingRoot: j.StagingDir, RollbackRoot: j.RollbackPath, ExpectedDigest: j.ExpectedDigest, TargetDigest: j.TargetDigest, Mode: j.TransactionMode}
				if tx.Mode == "" {
					tx.Mode = "atomic-exchange"
				}
				if err = candidate.NewTransactionCoordinator().Apply(ctx, tx, rewindJournalAdapter{store: s}); err != nil {
					return s.blockRewind(ctx, j, err)
				}
				if err = s.FinalizeRewind(ctx, j); err != nil {
					return s.blockRewind(ctx, j, err)
				}
				_ = os.RemoveAll(j.StagingDir)
				return nil
			} else {
				// The staging directory was never a valid target; discard it.
				_ = os.RemoveAll(j.StagingDir)
				return s.SetRewindPhase(ctx, j.ID, j.Phase, RewindFinalized, "interrupted before swap")
			}
		}
		// The exchange never happened (or was rolled back): nothing to
		// finish, just clean the staging directory.
		_ = os.RemoveAll(j.StagingDir)
		return s.SetRewindPhase(ctx, j.ID, j.Phase, RewindFinalized, "interrupted before swap")
	case j.TargetDigest:
		if rec.Candidate.Status != "ready" {
			return s.blockRewind(ctx, j, fmt.Errorf("candidate status %q cannot finalize a rewind", rec.Candidate.Status))
		}
		if err = s.FinalizeRewind(ctx, j); err != nil {
			return s.blockRewind(ctx, j, err)
		}
		_ = os.RemoveAll(j.StagingDir)
		return nil
	default:
		return s.blockRewind(ctx, j, fmt.Errorf("candidate digest %s matches neither the expected nor the target digest", digest))
	}
}

type rewindJournalAdapter struct{ store *Store }

func (j rewindJournalAdapter) Advance(ctx context.Context, id string, from, to candidate.TransactionPhase, reason string) error {
	return j.store.SetRewindPhase(ctx, id, string(from), string(to), reason)
}

func (s *Store) blockRewind(ctx context.Context, j RewindJournal, cause error) error {
	if j.Phase != RewindBlocked {
		if err := s.SetRewindPhase(ctx, j.ID, j.Phase, RewindBlocked, cause.Error()); err != nil {
			return err
		}
	}
	return fmt.Errorf("rewind %s blocked during recovery: %w", j.ID, cause)
}
