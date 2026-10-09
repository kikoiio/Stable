//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/platform/proc"
)

func writerProcessFixture(t *testing.T) (*LifecycleService, string, string, Scope, Snapshot, WriterLease) {
	t.Helper()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	stateRoot := filepath.Join(parent, "state")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "process recovery")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "child-run")
	if err != nil {
		t.Fatal(err)
	}
	return service, stateRoot, formal, scope, created, lease
}

func startTrackedWriterProcess(t *testing.T, token string) (*exec.Cmd, proc.TrackedProcess, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "STABLE_WORKSPACE_PROCESS_TOKEN="+token)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started, err := proc.ProcessStartTime(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, proc.TrackedProcess{PID: cmd.Process.Pid, ProcessGroup: cmd.Process.Pid, StartTimeTicks: started, Token: token}, done
}

func reopenWorkspaceService(t *testing.T, stateRoot, formal string) *LifecycleService {
	t.Helper()
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestRestartStopsOnlyMatchingTrackedWriterAndSettlesWorkspace(t *testing.T) {
	service, stateRoot, formal, scope, created, lease := writerProcessFixture(t)
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, identity, processDone := startTrackedWriterProcess(t, token)
	identity.WorkspaceID, identity.RunID, identity.Generation = lease.WorkspaceID, lease.RunID, lease.Generation
	if err := service.RegisterWriterProcess(context.Background(), lease, identity); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := reopenWorkspaceService(t, stateRoot, formal)
	defer recovered.Close(context.Background())
	select {
	case <-processDone:
	case <-time.After(3 * time.Second):
		t.Fatal("matching tracked workspace process was not reaped during recovery")
	}
	got, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || got.State != StateKept || got.WriterRunID != "" || got.Error == "" {
		t.Fatalf("recovered writer snapshot=%+v err=%v", got, err)
	}
	record, err := recovered.store.Load(context.Background(), scope, created.ID)
	if err != nil || record.Operation.Process != nil || record.Operation.Phase != "complete" {
		t.Fatalf("recovered process journal=%+v err=%v", record.Operation, err)
	}
	cursor := got.Cursor
	if err := recovered.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveredAgain := reopenWorkspaceService(t, stateRoot, formal)
	defer recoveredAgain.Close(context.Background())
	afterRepeat, err := recoveredAgain.Get(context.Background(), scope, created.ID)
	if err != nil || afterRepeat.State != StateKept || afterRepeat.WriterRunID != "" || afterRepeat.Cursor != cursor {
		t.Fatalf("repeated recovery changed settled writer: %+v err=%v", afterRepeat, err)
	}
}

func TestRestartBlocksMismatchedTrackedWriterWithoutSignaling(t *testing.T) {
	service, stateRoot, formal, scope, created, lease := writerProcessFixture(t)
	const actual = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cmd, identity, processDone := startTrackedWriterProcess(t, actual)
	identity.WorkspaceID, identity.RunID, identity.Generation = lease.WorkspaceID, lease.RunID, lease.Generation
	identity.Token = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := service.RegisterWriterProcess(context.Background(), lease, identity); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := reopenWorkspaceService(t, stateRoot, formal)
	defer recovered.Close(context.Background())
	got, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || got.State != StateInterrupted || got.WriterRunID != lease.RunID || got.Error == "" {
		t.Fatalf("mismatched process was not retained as interrupted: %+v %v", got, err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("recovery signaled a process with a different token: %v", err)
	}
	_ = cmd.Process.Kill()
	select {
	case <-processDone:
	case <-time.After(time.Second):
		t.Fatal("test process cleanup timed out")
	}
}

func TestRestartKeepsWriterWithoutPersistedProcessIdentityInterrupted(t *testing.T) {
	service, stateRoot, formal, scope, created, lease := writerProcessFixture(t)
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := reopenWorkspaceService(t, stateRoot, formal)
	defer recovered.Close(context.Background())
	got, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || got.State != StateInterrupted || got.WriterRunID != lease.RunID || got.Error == "" {
		t.Fatalf("missing process identity was not retained as interrupted: %+v %v", got, err)
	}
	record, err := recovered.store.Load(context.Background(), scope, created.ID)
	if err != nil || record.Operation.Process != nil || record.Operation.Phase != "blocked" {
		t.Fatalf("missing identity journal=%+v err=%v", record.Operation, err)
	}
}

func TestRestartRetriesBlockedWriterWithPersistedProcessIdentity(t *testing.T) {
	service, stateRoot, formal, scope, created, lease := writerProcessFixture(t)
	const token = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	_, identity, processDone := startTrackedWriterProcess(t, token)
	identity.WorkspaceID, identity.RunID, identity.Generation = lease.WorkspaceID, lease.RunID, lease.Generation
	if err := service.RegisterWriterProcess(context.Background(), lease, identity); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Snapshot.State = StateBlocked
	record.Snapshot.Error = "previous stop could not confirm child exit"
	record.Operation.Phase = "blocked"
	if err := service.store.Save(context.Background(), scope, record, record.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := reopenWorkspaceService(t, stateRoot, formal)
	defer recovered.Close(context.Background())
	select {
	case <-processDone:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not retry the safely identified blocked writer")
	}
	got, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || got.State != StateKept || got.WriterRunID != "" {
		t.Fatalf("retried blocked writer snapshot=%+v err=%v", got, err)
	}
}

func TestWriterCannotSettleWhileProcessIdentityRemainsPersisted(t *testing.T) {
	service, _, _, scope, _, lease := writerProcessFixture(t)
	process := proc.TrackedProcess{
		PID: 999999, ProcessGroup: 999999, StartTimeTicks: 1, Token: strings.Repeat("c", 64),
		WorkspaceID: lease.WorkspaceID, RunID: lease.RunID, Generation: lease.Generation,
	}
	if err := service.RegisterWriterProcess(context.Background(), lease, process); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReleaseCompletedWriter(context.Background(), lease); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("writer settled with an uncleared process identity: %v", err)
	}
	record, err := service.store.Load(context.Background(), scope, lease.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Snapshot.State != StateBlocked || record.Operation.Process == nil || *record.Operation.Process != process {
		t.Fatalf("uncertain process identity was not retained in blocked state: %+v", record)
	}
}
