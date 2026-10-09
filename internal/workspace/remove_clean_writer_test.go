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

func TestRemoveCleanRejectsActiveWriterUntilReleasedAndUnbound(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)

	created, err := service.Create(ctx, scope, "clean remove waits for writer")
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
	paths := lease.Paths
	before, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != StateWriting || before.WriterRunID != lease.RunID || before.Generation != lease.Generation {
		t.Fatalf("writer lease was not active before removal: snapshot=%+v lease=%+v", before, lease)
	}

	if _, err := service.RemoveClean(ctx, scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("clean remove accepted an active writer lease: %v", err)
	}
	after, err := service.Get(ctx, scope, created.ID)
	if err != nil || after.State != before.State || after.WriterRunID != before.WriterRunID || after.Generation != before.Generation || after.Cursor != before.Cursor {
		t.Fatalf("rejected remove changed workspace state or lease: before=%+v after=%+v err=%v", before, after, err)
	}
	if bound, err := service.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("rejected remove changed session binding: bound=%q err=%v", bound, err)
	}
	content, err := os.ReadFile(filepath.Join(paths.Checkout, "baseline.txt"))
	if err != nil || string(content) != "baseline" {
		t.Fatalf("rejected remove changed or deleted checkout: content=%q err=%v", content, err)
	}
	if reservation, err := service.ReserveWriterWrite(ctx, lease, 1); err != nil {
		t.Fatalf("rejected remove invalidated the active writer lease: %v", err)
	} else {
		reservation.Release()
	}

	released, err := service.ReleaseCompletedWriter(ctx, lease)
	if err != nil || released.State != StateKept || released.WriterRunID != "" {
		t.Fatalf("writer release=%+v err=%v", released, err)
	}
	if _, err := service.RemoveClean(ctx, scope, created.ID); err == nil {
		t.Fatal("clean remove accepted a workspace that remained bound after writer release")
	}
	if bound, err := service.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("binding changed after removal was rejected for the bound workspace: bound=%q err=%v", bound, err)
	}
	if content, err := os.ReadFile(filepath.Join(paths.Checkout, "baseline.txt")); err != nil || string(content) != "baseline" {
		t.Fatalf("binding rejection changed checkout: content=%q err=%v", content, err)
	}

	if _, err := service.Exit(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if bound, err := service.Binding(scope); err != nil || bound != "" {
		t.Fatalf("workspace remained bound after exit: bound=%q err=%v", bound, err)
	}
	removed, err := service.RemoveClean(ctx, scope, created.ID)
	if err != nil || removed.State != StateRemoved {
		t.Fatalf("clean remove after release and unbind=%+v err=%v", removed, err)
	}
	if _, err := os.Stat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful clean remove retained checkout root: %v", err)
	}
}
