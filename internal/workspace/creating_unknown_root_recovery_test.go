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

func TestRecoverCreatingWithoutPersistedRootIdentityFailsClosed(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "workspace-state")
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "creating-unknown-root-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate"),
	}
	service, err := openServiceWithError(stateRoot, formal)
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.Create(ctx, scope, "known workspace remains queryable")
	if err != nil {
		t.Fatalf("create known workspace: %v", err)
	}
	unknownID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	unknown := Record{
		Version: journalVersion,
		Scope:   scope,
		Snapshot: Snapshot{
			ID: unknownID, Label: "interrupted create", SessionID: scope.SessionID,
			State: StateCreating, Generation: 1,
		},
		Operation: Operation{ID: operationID, Kind: "create", Phase: "intent", Generation: 1, UpdatedAt: time.Now().UTC()},
	}
	if err := service.store.save(unknown); err != nil {
		t.Fatalf("persist initial create intent: %v", err)
	}
	paths, err := service.layout.Paths(unknownID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.Root, 0700); err != nil {
		t.Fatalf("simulate allocated root before identity journal save: %v", err)
	}
	sentinel := filepath.Join(paths.Root, "unknown-root-sentinel")
	if err := os.WriteFile(sentinel, []byte("retain unknown root"), 0600); err != nil {
		t.Fatal(err)
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
		recovered, err := openServiceWithError(stateRoot, formal)
		if err != nil {
			t.Fatalf("restart %d open service: %v", restart, err)
		}
		got, err := recovered.Get(ctx, scope, unknownID)
		if err != nil {
			t.Fatalf("restart %d query unknown root: %v", restart, err)
		}
		if got.State != StateInterrupted || got.Error != unknownCreatingRootIdentityReason {
			t.Fatalf("restart %d unknown root state=%s reason=%q", restart, got.State, got.Error)
		}
		if _, err := recovered.Get(ctx, scope, other.ID); err != nil {
			t.Fatalf("restart %d could not query known workspace: %v", restart, err)
		}
		listed, err := recovered.List(ctx, scope, 0, 100)
		if err != nil || len(listed) != 2 {
			t.Fatalf("restart %d list=%+v err=%v, want both owned records", restart, listed, err)
		}
		for name, action := range map[string]func() error{
			"get path through enter": func() error { _, err := recovered.Enter(ctx, scope, unknownID); return err },
			"keep":                   func() error { _, err := recovered.Keep(ctx, scope, unknownID); return err },
			"clean remove":           func() error { _, err := recovered.RemoveClean(ctx, scope, unknownID); return err },
			"export":                 func() error { _, err := recovered.Export(ctx, scope, unknownID); return err },
			"preview":                func() error { _, err := recovered.Preview(ctx, scope, unknownID); return err },
			"same-ID create": func() error {
				_, err := recovered.CreateWithID(ctx, scope, "interrupted create", unknownID)
				return err
			},
		} {
			if err := action(); !errors.Is(err, ErrOwnership) && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("restart %d %s on unknown root returned %v", restart, name, err)
			}
		}
		if _, err := recovered.Create(ctx, scope, "unknown root keeps project quota blocked"); !errors.Is(err, ErrQuota) {
			t.Fatalf("restart %d create while unknown root is retained returned %v, want ErrQuota", restart, err)
		}
		rootAfter, err := os.Lstat(paths.Root)
		if err != nil || !os.SameFile(rootBefore, rootAfter) {
			t.Fatalf("restart %d changed unknown root identity: before=%v after=%v err=%v", restart, rootBefore, rootAfter, err)
		}
		if content, err := os.ReadFile(sentinel); err != nil || string(content) != "retain unknown root" {
			t.Fatalf("restart %d changed unknown root sentinel: content=%q err=%v", restart, content, err)
		}
		current, err := recovered.store.Load(ctx, scope, unknownID)
		if err != nil {
			t.Fatalf("restart %d load unknown journal: %v", restart, err)
		}
		if current.RootIdentity != (RootIdentity{}) || current.Operation.Kind != "create" || current.Operation.Phase != "blocked" {
			t.Fatalf("restart %d adopted or unblocked unknown root journal: %+v", restart, current)
		}
		if current.UsedBytes != DefaultLimits().MaxProjectBytes {
			t.Fatalf("restart %d did not reserve project quota for unknown root: used=%d want=%d", restart, current.UsedBytes, DefaultLimits().MaxProjectBytes)
		}
		if restart == 1 {
			first = current
		} else if current.Snapshot.Cursor != first.Snapshot.Cursor || current.Operation.ID != first.Operation.ID || current.UsedBytes != first.UsedBytes {
			t.Fatalf("second recovery changed unknown-root journal: first=%+v current=%+v", first, current)
		}
		if err := recovered.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func openServiceWithError(stateRoot, formal string) (*LifecycleService, error) {
	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		return nil, err
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{
		IdleGuard: idleWorkspaceGuard{}, Exporter: previewWorkspaceExporter{},
	})
	if err != nil {
		return nil, err
	}
	return service, nil
}
