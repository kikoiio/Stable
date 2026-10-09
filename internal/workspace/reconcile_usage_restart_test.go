//go:build linux || darwin

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/permission"
)

func TestRestartReconcilesOwnedWorkspaceUsageAndDigestIdempotently(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "usage-recovery-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}

	open := func() *LifecycleService {
		t.Helper()
		layout, err := NewLayout(stateRoot, formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		service, err := NewService(layout, Limits{}, ServiceDependencies{})
		if err != nil {
			_ = layout.Close()
			t.Fatal(err)
		}
		return service
	}

	service := open()
	created, err := service.Create(ctx, scope, "usage reconciliation")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := service.layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("newly retained user data")
	if err := os.WriteFile(filepath.Join(paths.Checkout, "user.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	stale, err := service.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.UsedBytes = 1
	stale.Snapshot.WorkspaceDigest = strings.Repeat("0", 64)
	stale.Snapshot.ChangedFiles = 0
	if err := service.store.Save(ctx, scope, stale, stale.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	recovered := open()
	record, err := recovered.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantUsage, err := DiskUsage(ctx, paths.Root, recovered.limits.Normalized())
	if err != nil {
		t.Fatal(err)
	}
	wantManifest, err := BuildManifest(ctx, paths.Checkout, recovered.limits.Normalized())
	if err != nil {
		t.Fatal(err)
	}
	if record.UsedBytes != wantUsage || record.UsedBytes <= 1 {
		t.Fatalf("startup usage=%d, want actual owned usage %d", record.UsedBytes, wantUsage)
	}
	if record.Snapshot.WorkspaceDigest != wantManifest.Digest || record.Snapshot.ChangedFiles != 1 {
		t.Fatalf("startup snapshot digest=%q changed=%d, want digest=%q changed=1", record.Snapshot.WorkspaceDigest, record.Snapshot.ChangedFiles, wantManifest.Digest)
	}
	firstCursor := record.Snapshot.Cursor
	if err := recovered.Close(ctx); err != nil {
		t.Fatal(err)
	}

	again := open()
	defer again.Close(ctx)
	second, err := again.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.UsedBytes != wantUsage || second.Snapshot.WorkspaceDigest != wantManifest.Digest ||
		second.Snapshot.ChangedFiles != 1 || second.Snapshot.Cursor != firstCursor {
		t.Fatalf("second restart changed reconciled record: first cursor=%d second=%+v", firstCursor, second)
	}
	if got, err := os.ReadFile(filepath.Join(paths.Checkout, "user.txt")); err != nil || string(got) != string(content) {
		t.Fatalf("recovery changed retained user data: got=%q err=%v", got, err)
	}
}
