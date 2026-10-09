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

func TestInterruptedPartialCreateUnaccountableUsageBlocksAndReservesQuota(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	limits := Limits{MaxProjectBytes: 16 << 10, MaxWorkspaceBytes: 32 << 10}
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "unaccountable-create-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"),
	}

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
	if _, err := service.store.Create(ctx, scope, id, "unaccountable partial create", operationID); err != nil {
		t.Fatalf("persist create intent and root identity: %v", err)
	}
	paths, err := service.layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside-sentinel")
	if err := os.WriteFile(outside, []byte("must not be followed"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(paths.Root, "partial-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("simulate unsafe partial materialization: %v", err)
	}
	rootBefore, err := os.Lstat(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var first Record
	for restart := 1; restart <= 2; restart++ {
		recovered := open()
		got, err := recovered.Get(ctx, scope, id)
		if err != nil {
			t.Fatalf("restart %d query workspace: %v", restart, err)
		}
		if got.State != StateBlocked || got.Error == "" {
			t.Fatalf("restart %d state=%s error=%q, want blocked with accounting reason", restart, got.State, got.Error)
		}
		current, err := recovered.store.Load(ctx, scope, id)
		if err != nil {
			t.Fatalf("restart %d load journal: %v", restart, err)
		}
		if current.UsedBytes != limits.MaxProjectBytes {
			t.Fatalf("restart %d conservative usage=%d, want full project budget %d", restart, current.UsedBytes, limits.MaxProjectBytes)
		}
		if current.Operation.Phase != "blocked" {
			t.Fatalf("restart %d operation phase=%q, want blocked", restart, current.Operation.Phase)
		}
		if _, err := recovered.budget.ReserveCreate("quota-probe", 1); !errors.Is(err, ErrQuota) {
			t.Fatalf("restart %d reservation with unknown usage returned %v, want ErrQuota", restart, err)
		}
		rootAfter, err := os.Lstat(paths.Root)
		if err != nil || !os.SameFile(rootBefore, rootAfter) {
			t.Fatalf("restart %d replaced the partial root: before=%v after=%v err=%v", restart, rootBefore, rootAfter, err)
		}
		linkInfo, err := os.Lstat(link)
		if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("restart %d did not retain unsafe partial entry: info=%v err=%v", restart, linkInfo, err)
		}
		if content, err := os.ReadFile(outside); err != nil || string(content) != "must not be followed" {
			t.Fatalf("restart %d changed outside target: content=%q err=%v", restart, content, err)
		}
		if restart == 1 {
			first = current
		} else if current.Snapshot.Cursor != first.Snapshot.Cursor || current.Operation.ID != first.Operation.ID || current.UsedBytes != first.UsedBytes || current.Snapshot.Error != first.Snapshot.Error {
			t.Fatalf("second recovery changed conservative accounting: first=%+v current=%+v", first, current)
		}
		if err := recovered.Close(ctx); err != nil {
			t.Fatalf("restart %d close service: %v", restart, err)
		}
	}
}
