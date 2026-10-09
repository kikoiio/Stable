//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInterruptedPartialCreateUsageIsChargedAfterRestart(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	limits := Limits{MaxProjectBytes: 16 << 10, MaxWorkspaceBytes: 32 << 10}
	scope := testScope()

	open := func() *LifecycleService {
		t.Helper()
		layout, err := NewLayout(stateRoot, formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		service, err := NewService(layout, limits, ServiceDependencies{})
		if err != nil {
			_ = layout.Close()
			t.Fatal(err)
		}
		return service
	}

	service := open()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.Create(ctx, scope, id, "partial create", operationID); err != nil {
		t.Fatalf("persist create intent and root identity: %v", err)
	}
	paths, err := service.layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, 7<<10)
	if err := os.WriteFile(filepath.Join(paths.Root, "partial-materialize.bin"), partial, 0600); err != nil {
		t.Fatalf("write partial materialization: %v", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	recovered := open()
	defer recovered.Close(ctx)
	got, err := recovered.Get(ctx, scope, id)
	if err != nil || got.State != StateInterrupted {
		t.Fatalf("recovered state=%+v err=%v, want interrupted partial create", got, err)
	}
	record, err := recovered.store.Load(ctx, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := DiskUsage(ctx, paths.Root, limits.Normalized())
	if err != nil {
		t.Fatalf("measure retained partial root: %v", err)
	}
	if record.UsedBytes != actual || record.UsedBytes < int64(len(partial)) {
		t.Fatalf("partial create usage=%d, actual=%d; retained bytes were not charged", record.UsedBytes, actual)
	}
	if _, err := recovered.budget.ReserveCreate("quota-probe", 8<<10); !errors.Is(err, ErrQuota) {
		t.Fatalf("reservation ignored recovered partial usage: %v, want ErrQuota", err)
	}
}
