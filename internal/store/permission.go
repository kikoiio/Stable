package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"stable/internal/permission"
)

func (s *Store) CreateApproval(ctx context.Context, r permission.ApprovalRequest) error {
	raw, err := json.Marshal(r.Operation)
	if err != nil {
		return err
	}
	authority, err := json.Marshal(r.Authority)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO approval_requests(id,run_id,session_id,goal_id,work_item_id,operation_json,authority_json,reason,operation_digest,scope_digest,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.RunID, r.SessionID, r.GoalID, r.WorkItemID, string(raw), string(authority), r.Reason, r.OperationDigest, r.ScopeDigest, r.Status, r.CreatedAt.UTC().Format(time.RFC3339Nano), r.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetApproval(ctx context.Context, id string) (permission.ApprovalRequest, error) {
	var r permission.ApprovalRequest
	var raw, authority, created, expires string
	err := s.db.QueryRowContext(ctx, `SELECT id,run_id,session_id,goal_id,work_item_id,operation_json,authority_json,reason,operation_digest,scope_digest,status,created_at,expires_at FROM approval_requests WHERE id=?`, id).Scan(&r.ID, &r.RunID, &r.SessionID, &r.GoalID, &r.WorkItemID, &raw, &authority, &r.Reason, &r.OperationDigest, &r.ScopeDigest, &r.Status, &created, &expires)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(raw), &r.Operation); err != nil {
		return r, err
	}
	if authority == "" {
		return r, errors.New("approval has no persisted authority; refusing to resolve")
	}
	if err = json.Unmarshal([]byte(authority), &r.Authority); err != nil {
		return r, err
	}
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return r, err
	}
	r.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	return r, err
}

func (s *Store) GetApprovalForOperation(ctx context.Context, runID, operationDigest, scopeDigest string) (permission.ApprovalRequest, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM approval_requests WHERE run_id=? AND operation_digest=? AND scope_digest=? ORDER BY created_at DESC,id DESC LIMIT 1`, runID, operationDigest, scopeDigest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return permission.ApprovalRequest{}, false, nil
	}
	if err != nil {
		return permission.ApprovalRequest{}, false, err
	}
	r, err := s.GetApproval(ctx, id)
	return r, err == nil, err
}

func (s *Store) ListPendingApprovals(ctx context.Context, sessionID string) ([]permission.ApprovalRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM approval_requests WHERE session_id=? AND status='pending' ORDER BY created_at,id`, sessionID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var out []permission.ApprovalRequest
	for _, id := range ids {
		r, getErr := s.GetApproval(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) ResolveApproval(ctx context.Context, id string, status permission.ApprovalStatus, scope, operation string, rule *permission.ExactRule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status=? WHERE id=? AND status='pending' AND scope_digest=? AND operation_digest=?`, status, id, scope, operation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("approval is stale or already resolved")
	}
	if rule != nil {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO permission_rules(effect,kind,name,target,parameters_digest,scope_digest,protocol,host,port,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, rule.Effect, rule.Kind, rule.Name, rule.Target, rule.ParametersDigest, rule.ScopeDigest, rule.Protocol, rule.Host, rule.Port, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CancelApproval(ctx context.Context, id, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status='cancelled' WHERE id=? AND session_id=? AND status='pending'`, id, sessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("approval cannot be cancelled")
	}
	return nil
}

func (s *Store) ConsumeApproval(ctx context.Context, id, scope, operation string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET consumed_at=? WHERE id=? AND status='allowed_once' AND consumed_at IS NULL AND scope_digest=? AND operation_digest=?`, time.Now().UTC().Format(time.RFC3339Nano), id, scope, operation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("one-time approval is unavailable or already consumed")
	}
	return nil
}

func (s *Store) RecordPermissionDecision(ctx context.Context, d permission.PermissionDecision, a permission.Authority, o permission.Operation) error {
	if a.RunID == "" || a.SessionID == "" || o.ID == "" {
		return permission.ErrInvalidAuthority
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO permission_decisions(user_id,run_id,session_id,goal_id,work_item_id,operation_id,operation_digest,scope_digest,decision,reason,approval_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, d.UserID, a.RunID, a.SessionID, a.GoalID, a.WorkItemID, o.ID, d.OperationDigest, d.ScopeDigest, d.Kind, d.Reason, d.ApprovalID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListExactRules(ctx context.Context, scope string) ([]permission.ExactRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT effect,kind,name,target,parameters_digest,scope_digest,protocol,host,port FROM permission_rules WHERE scope_digest=? ORDER BY id`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []permission.ExactRule
	for rows.Next() {
		var r permission.ExactRule
		if err = rows.Scan(&r.Effect, &r.Kind, &r.Name, &r.Target, &r.ParametersDigest, &r.ScopeDigest, &r.Protocol, &r.Host, &r.Port); err != nil {
			return nil, err
		}
		if (r.Effect != permission.EffectAllow && r.Effect != permission.EffectDeny && r.Effect != permission.EffectAsk) || r.Kind == "" || r.Name == "" || r.ScopeDigest == "" {
			return nil, errors.New("permission rule is corrupt")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var _ permission.ApprovalRepository = (*Store)(nil)
