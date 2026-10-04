package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/artifact"
	"stable/internal/candidate"
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
	Record    CandidateRecord
	Decision  candidate.AcceptanceDecision
	Phase     string
	OldDigest string
	NewDigest string
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO candidates(id,action_id,goal_id,formal_root,candidate_root,baseline_digest,candidate_digest,root_mode,criteria_revision,dependency_revision,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Candidate.ID, r.ActionID, r.GoalID, r.Candidate.FormalRoot, r.Candidate.CandidateRoot, r.Candidate.BaselineDigest, r.Candidate.CandidateDigest, r.Candidate.RootMode, r.CriteriaRevision, r.DependencyRevision, r.Candidate.Status, r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetCandidate(ctx context.Context, id string) (CandidateRecord, error) {
	var r CandidateRecord
	var created, updated string
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT id,action_id,goal_id,formal_root,candidate_root,baseline_digest,candidate_digest,root_mode,criteria_revision,dependency_revision,status,created_at,updated_at FROM candidates WHERE id=?`, id).Scan(&r.Candidate.ID, &r.ActionID, &r.GoalID, &r.Candidate.FormalRoot, &r.Candidate.CandidateRoot, &r.Candidate.BaselineDigest, &r.Candidate.CandidateDigest, &r.Candidate.RootMode, &r.CriteriaRevision, &r.DependencyRevision, &status, &created, &updated)
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
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO acceptance_apply_journal(decision_id,candidate_id,phase,old_digest,new_digest,updated_at) VALUES(?,?,?,?,?,?)`, d.ID, d.CandidateID, "prepared", d.FormalDigest, d.CandidateDigest, time.Now().UTC().Format(time.RFC3339Nano))
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
func validAcceptanceTransition(from, to string) bool {
	return (from == "prepared" && (to == "swapped" || to == "blocked")) || (from == "swapped" && (to == "finalized" || to == "blocked"))
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
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.action_id,c.goal_id,c.formal_root,c.candidate_root,c.baseline_digest,c.candidate_digest,c.root_mode,c.criteria_revision,c.dependency_revision,c.status,c.created_at,c.updated_at,d.id,d.user_id,d.candidate_digest,d.preview_digest,d.formal_digest,d.mode,d.confirmed_findings_json,j.phase,j.old_digest,j.new_digest FROM acceptance_apply_journal j JOIN candidates c ON c.id=j.candidate_id JOIN acceptance_decisions d ON d.id=j.decision_id WHERE j.phase IN ('prepared','swapped','blocked') ORDER BY j.updated_at,j.decision_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AcceptanceRecovery
	for rows.Next() {
		var x AcceptanceRecovery
		var cstatus, created, updated, mode, findings string
		err = rows.Scan(&x.Record.Candidate.ID, &x.Record.ActionID, &x.Record.GoalID, &x.Record.Candidate.FormalRoot, &x.Record.Candidate.CandidateRoot, &x.Record.Candidate.BaselineDigest, &x.Record.Candidate.CandidateDigest, &x.Record.Candidate.RootMode, &x.Record.CriteriaRevision, &x.Record.DependencyRevision, &cstatus, &created, &updated, &x.Decision.ID, &x.Decision.UserID, &x.Decision.CandidateDigest, &x.Decision.PreviewDigest, &x.Decision.FormalDigest, &mode, &findings, &x.Phase, &x.OldDigest, &x.NewDigest)
		if err != nil {
			return nil, err
		}
		x.Record.Candidate.Status = cstatus
		x.Decision.CandidateID = x.Record.Candidate.ID
		x.Decision.Mode = candidate.AcceptanceMode(mode)
		if err = json.Unmarshal([]byte(findings), &x.Decision.ConfirmedFindings); err != nil {
			return nil, err
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
		_, formalDigest, err := candidate.BuildManifest(item.Record.Candidate.FormalRoot)
		if err != nil {
			return s.blockAcceptance(ctx, item, err)
		}
		_, candidateDigest, err := candidate.BuildManifest(item.Record.Candidate.CandidateRoot)
		if err != nil {
			return s.blockAcceptance(ctx, item, err)
		}
		oldDigest, newDigest := item.OldDigest, item.NewDigest
		if oldDigest == newDigest && formalDigest == newDigest && candidateDigest == newDigest {
			if item.Phase == "prepared" {
				if err = s.SetAcceptancePhase(ctx, item.Decision.ID, "prepared", "swapped", ""); err != nil {
					return err
				}
			}
		} else if formalDigest == oldDigest && candidateDigest == newDigest {
			if item.Phase != "prepared" {
				return s.blockAcceptance(ctx, item, errors.New("swapped journal points to old formal version"))
			}
			if err = candidate.ExchangeProjectDir(item.Record.Candidate.FormalRoot, item.Record.Candidate.CandidateRoot); err != nil {
				return s.blockAcceptance(ctx, item, err)
			}
			formalDigest = newDigest
			if err = s.SetAcceptancePhase(ctx, item.Decision.ID, "prepared", "swapped", ""); err != nil {
				return err
			}
		} else if formalDigest == newDigest && candidateDigest == oldDigest {
			if item.Phase == "prepared" {
				if err = s.SetAcceptancePhase(ctx, item.Decision.ID, "prepared", "swapped", ""); err != nil {
					return err
				}
			}
		} else {
			return s.blockAcceptance(ctx, item, fmt.Errorf("formal/candidate digests do not match the apply journal (formal=%s candidate=%s)", formalDigest, candidateDigest))
		}
		receipt := candidate.Receipt{ID: "receipt-" + item.Decision.ID, DecisionID: item.Decision.ID, CandidateID: item.Record.Candidate.ID, FormalDigest: newDigest, AcceptedAt: time.Now().UTC()}
		if err = s.FinalizeAcceptance(ctx, item.Decision, receipt, item.Record.GoalID, item.Record.ActionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) blockAcceptance(ctx context.Context, item AcceptanceRecovery, cause error) error {
	if item.Phase != "blocked" {
		if err := s.SetAcceptancePhase(ctx, item.Decision.ID, item.Phase, "blocked", cause.Error()); err != nil {
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
