package store

import (
	"context"
	"errors"
)

// DeleteEphemeralSessionRecords removes transient approval and decision rows
// for one session while leaving reusable permission rules untouched.
func (s *Store) DeleteEphemeralSessionRecords(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("session ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM approval_requests WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM permission_decisions WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
