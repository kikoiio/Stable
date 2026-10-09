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

type failingWorkspaceStopper struct{ err error }

func (s *failingWorkspaceStopper) StopWorkspaceWriter(ctx context.Context, _ WriterLease) error {
	if s.err == nil {
		return nil
	}
	if errors.Is(s.err, context.DeadlineExceeded) {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.err
}

func TestWorkspaceStopFailureRetainsBlockedLeaseForRetry(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	stopper := &failingWorkspaceStopper{err: errors.New("sandbox process did not exit")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{Stopper: stopper})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"), Mode: permission.ModeBypass}
	created, err := service.Create(context.Background(), scope, "stop failure")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "child")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.StopWriter(context.Background(), scope, created.ID); err == nil || err.Error() != "sandbox process did not exit" {
		t.Fatalf("StopWriter error=%v, want the stopper failure", err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Snapshot.State != StateBlocked || record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation || record.Operation.Phase != "blocked" {
		t.Fatalf("failed stop did not retain a blocked lease: %+v", record)
	}
	if _, err := service.ReserveWriterWrite(context.Background(), lease, 1); !errors.Is(err, ErrOwnership) {
		t.Fatalf("blocked writer accepted new writes: %v", err)
	}
	if _, err := service.RemoveClean(context.Background(), scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("clean remove accepted a workspace with an unconfirmed writer: %v", err)
	}

	// A later stop can retry the same durable lease once the process tree exits.
	stopper.err = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	settled, err := service.StopWriter(ctx, scope, created.ID)
	if err != nil || settled.State != StateKept || settled.WriterRunID != "" {
		t.Fatalf("retry stop=%+v err=%v", settled, err)
	}
}

func TestWorkspaceStopDeadlineRetainsBlockedLease(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	stopper := &failingWorkspaceStopper{err: context.DeadlineExceeded}
	service, err := NewService(layout, Limits{}, ServiceDependencies{Stopper: stopper})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"), Mode: permission.ModeBypass}
	created, err := service.Create(context.Background(), scope, "stop deadline")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "child")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.StopWriter(ctx, scope, created.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopWriter error=%v, want deadline exceeded", err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Snapshot.State != StateBlocked || record.Snapshot.WriterRunID != lease.RunID || record.Operation.Phase != "blocked" {
		t.Fatalf("deadline did not retain blocked lease: %+v", record)
	}
}
