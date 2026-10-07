//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
)

type settlingWriterStopper struct {
	service *LifecycleService
	pending WriteReservation
}

func (s *settlingWriterStopper) StopWorkspaceWriter(ctx context.Context, lease WriterLease) error {
	if _, err := s.service.ReserveWriterWrite(ctx, lease, 1); !errors.Is(err, ErrOwnership) {
		return errors.New("stopping allowed a new reservation")
	}
	if err := s.pending.Commit(ctx); err != nil {
		return err
	}
	got, err := s.service.ReleaseCompletedWriter(ctx, lease)
	if err != nil {
		return err
	}
	if got.State != StateStopping {
		return errors.New("terminal released a stopping lease early")
	}
	return nil
}
func TestWorkspaceStopDoesNotDeadlockPendingToolSettlement(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(root, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	stopper := &settlingWriterStopper{}
	service, err := NewService(layout, Limits{}, ServiceDependencies{Stopper: stopper})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	stopper.service = service
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(root, "candidate"), Mode: permission.ModeBypass}
	created, err := service.Create(context.Background(), scope, "stop")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "writer")
	if err != nil {
		t.Fatal(err)
	}
	stopper.pending, err = service.ReserveWriterWrite(context.Background(), lease, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Paths.Checkout, "new.txt"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := service.StopWriter(ctx, scope, created.ID)
	if err != nil || got.State != StateKept || got.WriterRunID != "" || got.ChangedFiles != 1 {
		t.Fatalf("stop=%+v err=%v", got, err)
	}
	if got, err := service.ReleaseCompletedWriter(ctx, lease); err != nil || got.State != StateKept {
		t.Fatalf("late terminal was not idempotent: %+v %v", got, err)
	}
}
