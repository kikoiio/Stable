//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/platform/proc"
)

func TestRemoveRetainsBindingAndLiveTrackedWriterAfterExitFailure(t *testing.T) {
	ctx := context.Background()
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
	service, err := NewService(layout, Limits{}, ServiceDependencies{
		IdleGuard: idleWorkspaceGuard{},
		Stopper:   &failingWorkspaceStopper{err: stopErr},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)

	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal,
		FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"),
	}
	created, err := service.Create(ctx, scope, "retain live writer after failed exit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(ctx, scope, created.ID, "child-writer")
	if err != nil {
		t.Fatal(err)
	}

	const token = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	cmd, identity, processDone := startTrackedWriterProcess(t, token)
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Kill()
		select {
		case <-processDone:
		case <-time.After(3 * time.Second):
			t.Errorf("controlled writer helper did not exit during cleanup")
		}
	})
	identity.WorkspaceID, identity.RunID, identity.Generation = lease.WorkspaceID, lease.RunID, lease.Generation
	if err := service.RegisterWriterProcess(ctx, lease, identity); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Exit(ctx, scope); !errors.Is(err, stopErr) {
		t.Fatalf("Exit error=%v, want the failed child-exit confirmation", err)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("controlled writer process exited despite failed stop: %v", err)
	}
	if start, err := proc.ProcessStartTime(cmd.Process.Pid); err != nil || start != identity.StartTimeTicks {
		t.Fatalf("controlled writer identity changed after failed stop: start=%d want=%d err=%v", start, identity.StartTimeTicks, err)
	}
	if bound, err := service.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("failed exit cleared workspace binding: bound=%q err=%v", bound, err)
	}

	blocked, err := service.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Snapshot.State != StateBlocked || blocked.Snapshot.WriterRunID != lease.RunID || blocked.Snapshot.Generation != lease.Generation || blocked.Operation.Kind != "stop" || blocked.Operation.Phase != "blocked" || blocked.Operation.Process == nil || *blocked.Operation.Process != identity || !strings.Contains(blocked.Snapshot.Error, stopErr.Error()) {
		t.Fatalf("failed child exit was not durably retained: snapshot=%+v operation=%+v", blocked.Snapshot, blocked.Operation)
	}

	if _, err := service.RemoveClean(ctx, scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("clean removal accepted a blocked live writer: %v", err)
	}
	if _, err := service.RemoveDiscardUser(ctx, scope, created.ID, "actual-user", "stale-decision", strings.Repeat("a", 64), lease.Generation); !errors.Is(err, ErrOwnership) {
		t.Fatalf("discard removal accepted a blocked live writer: %v", err)
	}
	if _, err := os.Stat(lease.Paths.Root); err != nil {
		t.Fatalf("failed exit/removal deleted the workspace root: %v", err)
	}
	if bound, err := service.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("removal rejection changed workspace binding: bound=%q err=%v", bound, err)
	}
	after, err := service.store.Load(ctx, scope, created.ID)
	if err != nil || after.Snapshot.State != StateBlocked || after.Operation.Process == nil || *after.Operation.Process != identity || after.Operation.Phase != "blocked" {
		t.Fatalf("rejected removal changed the durable live-writer record: snapshot=%+v operation=%+v err=%v", after.Snapshot, after.Operation, err)
	}
}
