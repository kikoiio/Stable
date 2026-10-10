//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
)

func TestIndependentSessionBindingsHoldIsolatedConcurrentWriters(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}

	firstScope := testScope()
	firstScope.Authority = permission.Authority{
		RunID: "session-one-lead", SessionID: firstScope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"),
	}
	secondScope := firstScope
	secondScope.SessionID = "session-two"
	secondScope.Work.SessionID = secondScope.SessionID
	secondScope.Authority.RunID = "session-two-lead"
	secondScope.Authority.SessionID = secondScope.SessionID
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	firstWorkspace, err := service.Create(context.Background(), firstScope, "session one workspace")
	if err != nil {
		t.Fatal(err)
	}
	secondWorkspace, err := service.Create(context.Background(), secondScope, "session two workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(context.Background(), firstScope, firstWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(context.Background(), secondScope, secondWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []struct {
		scope Scope
		want  string
	}{
		{firstScope, firstWorkspace.ID},
		{secondScope, secondWorkspace.ID},
	} {
		got, err := service.Binding(binding.scope)
		if err != nil || got != binding.want {
			t.Fatalf("session %s binding=%q, err=%v; want %q", binding.scope.SessionID, got, err, binding.want)
		}
	}
	if _, err := service.Get(context.Background(), firstScope, secondWorkspace.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("session one read session two's active workspace: %v", err)
	}
	if _, err := service.Get(context.Background(), secondScope, firstWorkspace.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("session two read session one's active workspace: %v", err)
	}

	firstLease, err := service.AcquireWriter(context.Background(), firstScope, firstWorkspace.ID, "child-one")
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := service.AcquireWriter(context.Background(), secondScope, secondWorkspace.ID, "child-two")
	if err != nil {
		t.Fatal(err)
	}
	if firstLease.WorkspaceID == secondLease.WorkspaceID || firstLease.Generation == 0 || secondLease.Generation == 0 ||
		firstLease.Authority.SessionID != firstScope.SessionID || secondLease.Authority.SessionID != secondScope.SessionID {
		t.Fatalf("leases did not preserve independent workspace/session identity: first=%+v second=%+v", firstLease, secondLease)
	}

	for _, writer := range []struct {
		lease WriterLease
		body  string
	}{{firstLease, "session one bytes"}, {secondLease, "session two bytes"}} {
		reservation, err := service.ReserveWriterWrite(context.Background(), writer.lease, int64(len(writer.body)+(64<<10)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(writer.lease.Paths.Checkout, "same-name.txt"), []byte(writer.body), 0600); err != nil {
			reservation.Release()
			t.Fatal(err)
		}
		if err := reservation.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		lease WriterLease
		scope Scope
		body  string
	}{
		{firstLease, firstScope, "session one bytes"},
		{secondLease, secondScope, "session two bytes"},
	} {
		got, err := os.ReadFile(filepath.Join(check.lease.Paths.Checkout, "same-name.txt"))
		if err != nil || string(got) != check.body {
			t.Fatalf("workspace %s shared or lost same-name content: %q err=%v want=%q", check.lease.WorkspaceID, got, err, check.body)
		}
		active, err := service.Get(context.Background(), check.scope, check.lease.WorkspaceID)
		if err != nil || active.State != StateWriting || active.WriterRunID != check.lease.RunID || active.Generation != check.lease.Generation {
			t.Fatalf("writer lease changed while sibling session wrote: snapshot=%+v err=%v lease=%+v", active, err, check.lease)
		}
	}

	for _, lease := range []WriterLease{firstLease, secondLease} {
		kept, err := service.ReleaseCompletedWriter(context.Background(), lease)
		if err != nil || kept.State != StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 1 {
			t.Fatalf("session writer did not settle independently: snapshot=%+v err=%v", kept, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(formal, "base.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("concurrent session writers changed formal root: %q err=%v", got, err)
	}
}
