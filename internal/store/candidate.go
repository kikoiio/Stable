package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/artifact"
	"stable/internal/candidate"
	"stable/internal/platform/secfile"
)

type CandidateRecord struct {
	Candidate          candidate.Candidate
	ActionID           string
	GoalID             string
	CriteriaRevision   int
	DependencyRevision int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type AcceptanceRecovery struct {
	Record               CandidateRecord
	Decision             candidate.AcceptanceDecision
	Phase                string
	OldDigest            string
	NewDigest            string
	TransactionMode      string
	RollbackPath         string
	ManifestPolicy       string
	ProtectedMetadata    []candidate.ProtectedMetadataFact
	ExpectedRootIdentity string
	TargetRootIdentity   string
}

func (s *Store) CheckAcceptance(ctx context.Context, d candidate.AcceptanceDecision) (bool, candidate.Receipt, bool, error) {
	findings, err := json.Marshal(d.ConfirmedFindings)
	if err != nil {
		return false, candidate.Receipt{}, false, err
	}
	var candidateID, userID, candidateDigest, previewDigest, formalDigest, mode, storedFindings string
	err = s.db.QueryRowContext(ctx, `SELECT candidate_id,user_id,candidate_digest,preview_digest,formal_digest,mode,confirmed_findings_json FROM acceptance_decisions WHERE id=?`, d.ID).Scan(&candidateID, &userID, &candidateDigest, &previewDigest, &formalDigest, &mode, &storedFindings)
	if errors.Is(err, sql.ErrNoRows) {
		return false, candidate.Receipt{}, false, nil
	}
	if err != nil {
		return false, candidate.Receipt{}, false, err
	}
	if candidateID != d.CandidateID || userID != d.UserID || candidateDigest != d.CandidateDigest || previewDigest != d.PreviewDigest || formalDigest != d.FormalDigest || mode != string(d.Mode) || storedFindings != string(findings) {
		return false, candidate.Receipt{}, false, errors.New("acceptance decision ID was reused with different contents")
	}
	r, ok, err := s.FindAcceptanceReceipt(ctx, d.ID)
	return true, r, ok, err
}

func (s *Store) SaveCandidate(ctx context.Context, r CandidateRecord) error {
	if r.Candidate.ID == "" || r.ActionID == "" || r.GoalID == "" {
		return errors.New("candidate attribution is incomplete")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	policy := r.Candidate.ManifestPolicy
	if policy == "" {
		policy = candidate.ManifestPolicyLegacy
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO candidates(id,action_id,goal_id,formal_root,candidate_root,manifest_policy,baseline_digest,candidate_digest,root_mode,criteria_revision,dependency_revision,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Candidate.ID, r.ActionID, r.GoalID, r.Candidate.FormalRoot, r.Candidate.CandidateRoot, policy, r.Candidate.BaselineDigest, r.Candidate.CandidateDigest, r.Candidate.RootMode, r.CriteriaRevision, r.DependencyRevision, r.Candidate.Status, r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetCandidate(ctx context.Context, id string) (CandidateRecord, error) {
	var r CandidateRecord
	var created, updated string
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT id,action_id,goal_id,formal_root,candidate_root,manifest_policy,baseline_digest,candidate_digest,root_mode,criteria_revision,dependency_revision,status,created_at,updated_at FROM candidates WHERE id=?`, id).Scan(&r.Candidate.ID, &r.ActionID, &r.GoalID, &r.Candidate.FormalRoot, &r.Candidate.CandidateRoot, &r.Candidate.ManifestPolicy, &r.Candidate.BaselineDigest, &r.Candidate.CandidateDigest, &r.Candidate.RootMode, &r.CriteriaRevision, &r.DependencyRevision, &status, &created, &updated)
	if err != nil {
		return r, err
	}
	r.Candidate.Status = status
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return r, err
	}
	r.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return r, err
}

func (s *Store) TransitionCandidate(ctx context.Context, id, from, to, digest string) error {
	if !validCandidateTransition(from, to) {
		return fmt.Errorf("invalid candidate transition %s -> %s", from, to)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE candidates SET status=?,candidate_digest=CASE WHEN ?='' THEN candidate_digest ELSE ? END,updated_at=? WHERE id=? AND status=?`, to, digest, digest, time.Now().UTC().Format(time.RFC3339Nano), id, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("candidate state changed")
	}
	return nil
}

func validCandidateTransition(from, to string) bool {
	switch from + "->" + to {
	case "prepared->running", "prepared->ready", "prepared->blocked", "running->ready", "running->blocked", "ready->reviewed", "ready->blocked", "reviewed->accepted", "reviewed->rejected", "reviewed->blocked":
		return true
	}
	return false
}

func (s *Store) SaveCandidateReview(ctx context.Context, r candidate.Review) error {
	if r.ID == "" || r.CandidateID == "" || r.Digest == "" {
		return errors.New("review identity is incomplete")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO candidate_reviews(id,candidate_id,formal_digest,candidate_digest,preview_digest,review_json,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET formal_digest=excluded.formal_digest,candidate_digest=excluded.candidate_digest,preview_digest=excluded.preview_digest,review_json=excluded.review_json,created_at=excluded.created_at`, r.ID, r.CandidateID, r.FormalDigest, r.CandidateDigest, r.Digest, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetCandidateReview(ctx context.Context, id string) (candidate.Review, error) {
	var r candidate.Review
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT review_json FROM candidate_reviews WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(raw), &r)
	return r, err
}

func (s *Store) SaveAcceptanceDecision(ctx context.Context, d candidate.AcceptanceDecision) (bool, error) {
	if d.ID == "" || d.UserID == "" || d.CandidateID == "" {
		return false, errors.New("acceptance decision attribution is incomplete")
	}
	findings, err := json.Marshal(d.ConfirmedFindings)
	if err != nil {
		return false, err
	}
	var candidateRoot, formalRoot, manifestPolicy string
	if err = s.db.QueryRowContext(ctx, `SELECT candidate_root,formal_root,manifest_policy FROM candidates WHERE id=?`, d.CandidateID).Scan(&candidateRoot, &formalRoot, &manifestPolicy); err != nil {
		return false, err
	}
	var expectedRootIdentity, targetRootIdentity string
	if manifestPolicy == candidate.ManifestPolicyProject {
		expectedRootIdentity, err = candidate.CaptureRootIdentity(formalRoot)
		if err != nil {
			return false, err
		}
		targetRootIdentity, err = candidate.CaptureRootIdentity(candidateRoot)
		if err != nil {
			return false, err
		}
	}
	transactionMode := secfile.TransactionMode()
	rollback := filepath.Join(filepath.Dir(filepath.Clean(candidateRoot)), ".stable-accept-rollback-"+d.ID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO acceptance_decisions(id,candidate_id,user_id,candidate_digest,preview_digest,formal_digest,mode,confirmed_findings_json,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, d.ID, d.CandidateID, d.UserID, d.CandidateDigest, d.PreviewDigest, d.FormalDigest, d.Mode, string(findings), "prepared", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	createdCount, _ := res.RowsAffected()
	var candidateID, userID, candidateDigest, previewDigest, formalDigest, mode, storedFindings, status string
	if err = tx.QueryRowContext(ctx, `SELECT candidate_id,user_id,candidate_digest,preview_digest,formal_digest,mode,confirmed_findings_json,status FROM acceptance_decisions WHERE id=?`, d.ID).Scan(&candidateID, &userID, &candidateDigest, &previewDigest, &formalDigest, &mode, &storedFindings, &status); err != nil {
		return false, err
	}
	if candidateID != d.CandidateID || userID != d.UserID || candidateDigest != d.CandidateDigest || previewDigest != d.PreviewDigest || formalDigest != d.FormalDigest || mode != string(d.Mode) || storedFindings != string(findings) {
		return false, errors.New("acceptance decision ID was reused with different contents")
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO acceptance_apply_journal(decision_id,candidate_id,phase,transaction_mode,rollback_path,manifest_policy,expected_root_identity,target_root_identity,old_digest,new_digest,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.CandidateID, "prepared", transactionMode, rollback, manifestPolicy, expectedRootIdentity, targetRootIdentity, d.FormalDigest, d.CandidateDigest, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return createdCount == 1, nil
}

func (s *Store) SetAcceptancePhase(ctx context.Context, decisionID, from, to, reason string) error {
	if !validAcceptanceTransition(from, to) {
		return errors.New("invalid acceptance phase")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE acceptance_apply_journal SET phase=?,reason=?,updated_at=? WHERE decision_id=? AND phase=?`, to, reason, time.Now().UTC().Format(time.RFC3339Nano), decisionID, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("acceptance phase changed")
	}
	return nil
}

// AcceptanceTransaction returns the persisted platform mode and rollback
// path. It is intentionally a small optional hook so older in-memory callers
// can keep implementing AcceptanceStore without journal metadata.
func (s *Store) AcceptanceTransaction(ctx context.Context, decisionID string) (string, string, error) {
	var mode, rollback string
	err := s.db.QueryRowContext(ctx, `SELECT transaction_mode,rollback_path FROM acceptance_apply_journal WHERE decision_id=?`, decisionID).Scan(&mode, &rollback)
	return mode, rollback, err
}

func (s *Store) SaveProtectedMetadata(ctx context.Context, decisionID string, facts []candidate.ProtectedMetadataFact) error {
	if len(facts) != 3 {
		return errors.New("protected metadata facts are incomplete")
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE acceptance_apply_journal SET protected_metadata_json=? WHERE decision_id=? AND phase='prepared' AND manifest_policy=? AND protected_metadata_json=''`, string(raw), decisionID, candidate.ManifestPolicyProject)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return nil
	}
	var existing string
	if err = s.db.QueryRowContext(ctx, `SELECT protected_metadata_json FROM acceptance_apply_journal WHERE decision_id=?`, decisionID).Scan(&existing); err != nil {
		return err
	}
	if existing != string(raw) {
		return errors.New("protected metadata facts changed for acceptance")
	}
	return nil
}

func (s *Store) LoadProtectedMetadata(ctx context.Context, decisionID string) ([]candidate.ProtectedMetadataFact, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT protected_metadata_json FROM acceptance_apply_journal WHERE decision_id=?`, decisionID).Scan(&raw); err != nil {
		return nil, err
	}
	var facts []candidate.ProtectedMetadataFact
	if raw == "" {
		return nil, errors.New("protected metadata facts are missing")
	}
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil, err
	}
	if len(facts) != 3 {
		return nil, errors.New("protected metadata facts are incomplete")
	}
	return facts, nil
}
func validAcceptanceTransition(from, to string) bool {
	return (from == "prepared" && (to == "old_saved" || to == "swapped" || to == "blocked")) ||
		(from == "old_saved" && (to == "target_installed" || to == "blocked")) ||
		(from == "target_installed" && (to == "swapped" || to == "blocked")) ||
		(from == "swapped" && (to == "finalized" || to == "blocked"))
}

func (s *Store) GetAcceptanceReceipt(ctx context.Context, decisionID string) (candidate.Receipt, error) {
	var r candidate.Receipt
	var received string
	err := s.db.QueryRowContext(ctx, `SELECT id,decision_id,candidate_id,formal_digest,received_at FROM acceptance_receipts WHERE decision_id=?`, decisionID).Scan(&r.ID, &r.DecisionID, &r.CandidateID, &r.FormalDigest, &received)
	if err != nil {
		return r, err
	}
	r.AcceptedAt, err = time.Parse(time.RFC3339Nano, received)
	return r, err
}

func (s *Store) FindAcceptanceReceipt(ctx context.Context, decisionID string) (candidate.Receipt, bool, error) {
	r, err := s.GetAcceptanceReceipt(ctx, decisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return candidate.Receipt{}, false, nil
	}
	return r, err == nil, err
}

func (s *Store) PendingAcceptances(ctx context.Context) ([]AcceptanceRecovery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.action_id,c.goal_id,c.formal_root,c.candidate_root,c.manifest_policy,c.baseline_digest,c.candidate_digest,c.root_mode,c.criteria_revision,c.dependency_revision,c.status,c.created_at,c.updated_at,d.id,d.user_id,d.candidate_digest,d.preview_digest,d.formal_digest,d.mode,d.confirmed_findings_json,j.phase,j.old_digest,j.new_digest,j.transaction_mode,j.rollback_path,j.manifest_policy,j.expected_root_identity,j.target_root_identity,j.protected_metadata_json FROM acceptance_apply_journal j JOIN candidates c ON c.id=j.candidate_id JOIN acceptance_decisions d ON d.id=j.decision_id WHERE j.phase IN ('prepared','old_saved','target_installed','swapped','blocked') ORDER BY j.updated_at,j.decision_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AcceptanceRecovery
	for rows.Next() {
		var x AcceptanceRecovery
		var cstatus, created, updated, mode, findings string
		var metadataRaw string
		err = rows.Scan(&x.Record.Candidate.ID, &x.Record.ActionID, &x.Record.GoalID, &x.Record.Candidate.FormalRoot, &x.Record.Candidate.CandidateRoot, &x.Record.Candidate.ManifestPolicy, &x.Record.Candidate.BaselineDigest, &x.Record.Candidate.CandidateDigest, &x.Record.Candidate.RootMode, &x.Record.CriteriaRevision, &x.Record.DependencyRevision, &cstatus, &created, &updated, &x.Decision.ID, &x.Decision.UserID, &x.Decision.CandidateDigest, &x.Decision.PreviewDigest, &x.Decision.FormalDigest, &mode, &findings, &x.Phase, &x.OldDigest, &x.NewDigest, &x.TransactionMode, &x.RollbackPath, &x.ManifestPolicy, &x.ExpectedRootIdentity, &x.TargetRootIdentity, &metadataRaw)
		if err != nil {
			return nil, err
		}
		x.Record.Candidate.Status = cstatus
		x.Decision.CandidateID = x.Record.Candidate.ID
		x.Decision.Mode = candidate.AcceptanceMode(mode)
		if err = json.Unmarshal([]byte(findings), &x.Decision.ConfirmedFindings); err != nil {
			return nil, err
		}
		if metadataRaw != "" {
			if err = json.Unmarshal([]byte(metadataRaw), &x.ProtectedMetadata); err != nil {
				return nil, err
			}
		}
		if x.Record.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if x.Record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) ReconcileAcceptances(ctx context.Context) error {
	items, err := s.PendingAcceptances(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Phase == "blocked" {
			continue
		}
		policy := item.ManifestPolicy
		if policy == "" {
			policy = candidate.ManifestPolicyLegacy
		}
		if item.Record.Candidate.ManifestPolicy != policy {
			return s.blockAcceptance(ctx, item, errors.New("candidate and acceptance journal manifest policies differ"))
		}
		if policy == candidate.ManifestPolicyProject && len(item.ProtectedMetadata) != 3 {
			return s.blockAcceptance(ctx, item, errors.New("protected metadata facts are missing from the acceptance journal"))
		}
		oldDigest, newDigest := item.OldDigest, item.NewDigest
		tx := candidate.DirectoryTransaction{ID: item.Decision.ID, Kind: candidate.TransactionAcceptance, ManifestPolicy: policy, ProtectedMetadata: item.ProtectedMetadata, ExpectedRootIdentity: item.ExpectedRootIdentity, TargetRootIdentity: item.TargetRootIdentity, CurrentRoot: item.Record.Candidate.FormalRoot, IncomingRoot: item.Record.Candidate.CandidateRoot, RollbackRoot: item.RollbackPath, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: item.TransactionMode}
		if tx.Mode == "" {
			tx.Mode = "atomic-exchange"
		}
		coordinator := candidate.NewTransactionCoordinator()
		state, inspectErr := coordinator.Inspect(ctx, tx, candidate.TransactionPhase(item.Phase))
		if inspectErr != nil {
			return s.blockAcceptance(ctx, item, inspectErr)
		}
		if state == candidate.RecoveryOld && item.Phase == "prepared" {
			err = coordinator.Apply(ctx, tx, acceptanceJournalAdapter{store: s})
		} else {
			err = coordinator.Recover(ctx, tx, candidate.TransactionPhase(item.Phase), acceptanceJournalAdapter{store: s})
		}
		if err != nil {
			return s.blockAcceptance(ctx, item, err)
		}

		if policy == candidate.ManifestPolicyProject {
			err = candidate.RestoreProtectedMetadataFacts(item.Record.Candidate.FormalRoot, item.Record.Candidate.CandidateRoot, item.ProtectedMetadata)
		} else {
			err = candidate.RestoreServiceRoot(item.Record.Candidate.FormalRoot, item.Record.Candidate.CandidateRoot)
		}
		if err != nil {
			return s.blockAcceptance(ctx, item, err)
		}
		if state, inspectErr := coordinator.Inspect(ctx, tx, candidate.PhaseSwapped); inspectErr != nil || state != candidate.RecoveryNew {
			if inspectErr == nil {
				inspectErr = errors.New("acceptance roots changed before finalization")
			}
			return s.blockAcceptance(ctx, item, inspectErr)
		}
		receipt := candidate.Receipt{ID: "receipt-" + item.Decision.ID, DecisionID: item.Decision.ID, CandidateID: item.Record.Candidate.ID, FormalDigest: newDigest, AcceptedAt: time.Now().UTC()}
		if err = s.FinalizeAcceptance(ctx, item.Decision, receipt, item.Record.GoalID, item.Record.ActionID); err != nil {
			return err
		}
	}
	// Acceptance finalization commits the receipt and candidate state before
	// deleting the spent root. A crash in that window leaves a finalized
	// journal, so recover cleanup only when the receipt and current formal root
	// prove that this exact acceptance committed.
	type finalizedAcceptance struct {
		decisionID, candidateID, formalRoot, candidateRoot  string
		candidatePolicy, candidateDigest, candidateStatus   string
		oldDigest, newDigest, transactionMode, rollbackPath string
		journalPolicy, expectedIdentity, targetIdentity     string
		receiptCandidateID, receiptDigest                   string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT j.decision_id,j.candidate_id,c.formal_root,c.candidate_root,c.manifest_policy,c.candidate_digest,c.status,j.old_digest,j.new_digest,j.transaction_mode,j.rollback_path,j.manifest_policy,j.expected_root_identity,j.target_root_identity,r.candidate_id,r.formal_digest FROM acceptance_apply_journal j JOIN candidates c ON c.id=j.candidate_id LEFT JOIN acceptance_receipts r ON r.decision_id=j.decision_id WHERE j.phase='finalized' AND j.manifest_policy=? AND j.expected_root_identity<>'' ORDER BY j.updated_at,j.decision_id`, candidate.ManifestPolicyProject)
	if err != nil {
		return err
	}
	var finalized []finalizedAcceptance
	for rows.Next() {
		var item finalizedAcceptance
		if err = rows.Scan(&item.decisionID, &item.candidateID, &item.formalRoot, &item.candidateRoot, &item.candidatePolicy, &item.candidateDigest, &item.candidateStatus, &item.oldDigest, &item.newDigest, &item.transactionMode, &item.rollbackPath, &item.journalPolicy, &item.expectedIdentity, &item.targetIdentity, &item.receiptCandidateID, &item.receiptDigest); err != nil {
			rows.Close()
			return err
		}
		finalized = append(finalized, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, item := range finalized {
		if item.candidatePolicy != candidate.ManifestPolicyProject || item.candidateStatus != "accepted" || item.candidateDigest != item.newDigest || item.receiptCandidateID != item.candidateID || item.receiptDigest != item.newDigest {
			continue
		}
		tx := candidate.DirectoryTransaction{
			ID: item.decisionID, Kind: candidate.TransactionAcceptance, ManifestPolicy: item.journalPolicy,
			ExpectedRootIdentity: item.expectedIdentity, TargetRootIdentity: item.targetIdentity,
			CurrentRoot: item.formalRoot, IncomingRoot: item.candidateRoot, RollbackRoot: item.rollbackPath,
			ExpectedDigest: item.oldDigest, TargetDigest: item.newDigest, Mode: item.transactionMode,
		}
		coordinator := candidate.NewTransactionCoordinator()
		incomingErr := error(nil)
		rollbackErr := error(nil)
		if _, incomingErr = os.Lstat(tx.IncomingRoot); incomingErr != nil && !os.IsNotExist(incomingErr) {
			return incomingErr
		}
		if _, rollbackErr = os.Lstat(tx.RollbackRoot); rollbackErr != nil && !os.IsNotExist(rollbackErr) {
			return rollbackErr
		}
		if os.IsNotExist(incomingErr) && os.IsNotExist(rollbackErr) {
			identity, identityErr := candidate.CaptureRootIdentity(tx.CurrentRoot)
			_, digest, digestErr := candidate.BuildManifestForPolicy(tx.CurrentRoot, tx.ManifestPolicy)
			if identityErr != nil || digestErr != nil || identity != tx.TargetRootIdentity || digest != tx.TargetDigest {
				return errors.New("finalized acceptance roots no longer match receipt")
			}
			continue
		}
		state, inspectErr := coordinator.Inspect(ctx, tx, candidate.PhaseSwapped)
		if inspectErr != nil {
			return inspectErr
		}
		if state != candidate.RecoveryNew {
			return errors.New("finalized acceptance roots no longer match receipt")
		}
		if err = coordinator.Cleanup(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

type acceptanceJournalAdapter struct{ store *Store }

func (j acceptanceJournalAdapter) Advance(ctx context.Context, id string, from, to candidate.TransactionPhase, reason string) error {
	return j.store.SetAcceptancePhase(ctx, id, string(from), string(to), reason)
}

func (s *Store) blockAcceptance(ctx context.Context, item AcceptanceRecovery, cause error) error {
	var currentPhase string
	if err := s.db.QueryRowContext(ctx, `SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, item.Decision.ID).Scan(&currentPhase); err != nil {
		return err
	}
	if currentPhase != "blocked" {
		if err := s.SetAcceptancePhase(ctx, item.Decision.ID, currentPhase, "blocked", cause.Error()); err != nil {
			return err
		}
	}
	return fmt.Errorf("acceptance %s blocked during recovery: %w", item.Decision.ID, cause)
}

// FinalizeAcceptance records the unique receipt and schedules independent
// reverification in one SQLite transaction after the directory exchange.
func (s *Store) FinalizeAcceptance(ctx context.Context, d candidate.AcceptanceDecision, r candidate.Receipt, goalID, actionID string) error {
	artifactID := r.FormalDigest
	if !strings.HasPrefix(goalID, "session-") {
		var artifactPath string
		if err := s.db.QueryRowContext(ctx, `SELECT artifact_path FROM goals WHERE id=?`, goalID).Scan(&artifactPath); err != nil {
			return err
		}
		if artifactPath != "" {
			var err error
			artifactID, err = artifact.Digest(ctx, artifactPath)
			if err != nil {
				return err
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase string
	if err = tx.QueryRowContext(ctx, `SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil {
		return err
	}
	if phase == "finalized" {
		return tx.Commit()
	}
	if phase != "swapped" {
		return errors.New("acceptance was not swapped")
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO acceptance_receipts(id,decision_id,candidate_id,formal_digest,received_at) VALUES(?,?,?,?,?)`, r.ID, r.DecisionID, r.CandidateID, r.FormalDigest, r.AcceptedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE acceptance_apply_journal SET phase='finalized',updated_at=? WHERE decision_id=? AND phase='swapped'`, time.Now().UTC().Format(time.RFC3339Nano), d.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE acceptance_decisions SET status='applied' WHERE id=?`, d.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE candidates SET status='accepted',candidate_digest=?,updated_at=? WHERE id=?`, r.FormalDigest, time.Now().UTC().Format(time.RFC3339Nano), d.CandidateID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(goalID, "session-") {
		_, err = tx.ExecContext(ctx, `UPDATE goals SET current_artifact_id=?,status='pending_reverification',reason='candidate accepted; independent reverification required',revision=revision+1 WHERE id=?`, artifactID, goalID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE agents SET status=? WHERE goal_id=?`, agentStatusFor("pending_reverification"), goalID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE evidence SET result='stale',invalidated_reason='formal project changed by accepted candidate' WHERE goal_id=? AND result='pass'`, goalID); err != nil {
			return err
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=?`, "accept-"+d.ID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			payload, _ := json.Marshal(map[string]string{"decision_id": d.ID, "candidate_id": d.CandidateID})
			if _, err = tx.ExecContext(ctx, `INSERT INTO events(id,goal_id,kind,payload_json,received_at,status) VALUES(?,?,?,?,?,'pending')`, "accept-"+d.ID, goalID, "candidate_accepted", string(payload), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	if actionID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE actions SET status='applied',result_artifact_id=?,reason='candidate accepted; independent reverification required' WHERE id=?`, r.FormalDigest, actionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveAcceptanceRootIdentities(ctx context.Context, id, expected, target string) error {
	if expected == "" || target == "" {
		return errors.New("transaction root identities are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE acceptance_apply_journal SET expected_root_identity=?,target_root_identity=? WHERE decision_id=? AND phase='prepared' AND (expected_root_identity='' OR expected_root_identity=?) AND (target_root_identity='' OR target_root_identity=?)`, expected, target, id, expected, target)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("transaction root identities changed or journal is no longer prepared")
	}
	return nil
}
