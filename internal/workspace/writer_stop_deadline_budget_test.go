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

type recordingWorkspaceStopper struct {
	deadline time.Time
	observed time.Time
	err      error
}

func (s *recordingWorkspaceStopper) StopWorkspaceWriter(ctx context.Context, _ WriterLease) error {
	s.observed = time.Now()
	s.deadline, _ = ctx.Deadline()
	return s.err
}

func TestWorkspaceStopUsesBoundedDeadlineAndRetainsLeaseOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parentTimeout time.Duration
	}{
		{name: "production budget"},
		{name: "short parent", parentTimeout: 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			stopErr := errors.New("sandbox process exit was not confirmed")
			stopper := &recordingWorkspaceStopper{err: stopErr}
			service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}, Stopper: stopper})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())

			scope := testScope()
			scope.Authority = permission.Authority{RunID: "lead", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"), Mode: permission.ModeBypass}
			created, err := service.Create(context.Background(), scope, "stop deadline budget")
			if err != nil {
				t.Fatal(err)
			}
			lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "child")
			if err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			cancel := func() {}
			var parentDeadline time.Time
			if tc.parentTimeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, tc.parentTimeout)
				parentDeadline, _ = ctx.Deadline()
			}
			defer cancel()
			if _, err := service.StopWriter(ctx, scope, created.ID); !errors.Is(err, stopErr) {
				t.Fatalf("StopWriter error=%v, want stopper error", err)
			}
			if stopper.deadline.IsZero() {
				t.Fatal("stopper context has no deadline")
			}
			if tc.parentTimeout == 0 {
				if stopper.deadline.After(stopper.observed.Add(10 * time.Second)) {
					t.Fatalf("stopper deadline %v exceeds 10-second budget at observation %v", stopper.deadline, stopper.observed)
				}
			} else if !stopper.deadline.Equal(parentDeadline) {
				t.Fatalf("stopper deadline %v widened/changed shorter parent deadline %v", stopper.deadline, parentDeadline)
			}

			record, err := service.store.Load(context.Background(), scope, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Snapshot.State != StateBlocked || record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation || record.Operation.Phase != "blocked" {
				t.Fatalf("failed stop did not retain blocked lease: %+v", record)
			}
		})
	}
}
