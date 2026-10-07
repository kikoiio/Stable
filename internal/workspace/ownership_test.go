//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func ownershipFixture(t *testing.T) (*Layout, *OwnershipStore) {
	t.Helper()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewOwnershipStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	return layout, store
}

func TestOwnershipScopeGenerationAndReplacement(t *testing.T) {
	layout, store := ownershipFixture(t)
	ctx := context.Background()
	scope := testScope()
	r, err := store.Create(ctx, scope, "workspace", "a user label / unrelated to paths", "operation")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths("workspace")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.Journal)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal is not private: %v, %v", info, err)
	}
	wrong := scope
	wrong.SessionID, wrong.Work.SessionID = "other", "other"
	if _, err := store.Load(ctx, wrong, "workspace"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("cross-session load accepted: %v", err)
	}
	r.Snapshot.State, r.Operation.Phase = StateReady, "complete"
	if err := store.Save(ctx, scope, r, 1); err != nil {
		t.Fatal(err)
	}
	r.Snapshot.State, r.Snapshot.Generation, r.Operation.Generation = StateWriting, 2, 2
	r.Operation.ID, r.Operation.Kind = "writer", "write"
	if err := store.Save(ctx, scope, r, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, scope, r, 1); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	jump := r
	jump.Snapshot.Generation, jump.Operation.Generation = 4, 4
	if err := store.Save(ctx, scope, jump, 2); !errors.Is(err, ErrOwnership) {
		t.Fatalf("generation jump accepted: %v", err)
	}
	if err := os.Rename(paths.Root, paths.Root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, scope, "workspace"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("physical replacement adopted: %v", err)
	}
}

func TestOwnershipNeverAdoptsPreexistingRoot(t *testing.T) {
	layout, store := ownershipFixture(t)
	paths, err := layout.Allocate("unknown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), testScope(), "unknown", "same label", "create"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("preexisting directory adopted: %v", err)
	}
	if _, err := os.Stat(paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal synthesized for unknown root: %v", err)
	}
}

func TestOwnershipRemovedJournalSurvivesAndIsNotLive(t *testing.T) {
	layout, store := ownershipFixture(t)
	ctx := context.Background()
	scope := testScope()
	r, err := store.Create(ctx, scope, "workspace", "remove", "create")
	if err != nil {
		t.Fatal(err)
	}
	r.Snapshot.State, r.Operation.Kind = StateRemoving, "remove"
	if err := store.Save(ctx, scope, r, 1); err != nil {
		t.Fatal(err)
	}
	r.Snapshot.State, r.Operation.Phase = StateRemoved, "complete"
	if err := store.Save(ctx, scope, r, 1); err == nil {
		t.Fatal("existing root marked removed")
	}
	paths, _ := layout.Paths("workspace")
	if err := os.Remove(paths.Root); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, scope, r, 1); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, scope, "workspace")
	if err != nil || loaded.Snapshot.State != StateRemoved {
		t.Fatalf("removed fact disappeared: %+v, %v", loaded, err)
	}
	live, err := store.Records(ctx)
	if err != nil || len(live) != 0 {
		t.Fatalf("removed resource charged as live: %+v, %v", live, err)
	}
	count := 0
	if err := store.Scan(ctx, func(Record) error { count++; return nil }); err != nil || count != 1 {
		t.Fatalf("history not streamed: %d, %v", count, err)
	}
}

func TestOwnershipRejectsTamperingAndProjectReplacement(t *testing.T) {
	t.Run("journal mode", func(t *testing.T) {
		layout, store := ownershipFixture(t)
		if _, err := store.Create(context.Background(), testScope(), "work", "label", "create"); err != nil {
			t.Fatal(err)
		}
		paths, _ := layout.Paths("work")
		if err := os.Chmod(paths.Journal, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), testScope(), "work"); !errors.Is(err, ErrOwnership) {
			t.Fatalf("public journal accepted: %v", err)
		}
	})
	t.Run("project replacement", func(t *testing.T) {
		layout, _ := ownershipFixture(t)
		project := layout.projectRoot()
		if err := os.Rename(project, project+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(project, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := layout.Allocate("work"); err == nil {
			t.Fatal("replaced project directory used")
		}
	})
	t.Run("overlap", func(t *testing.T) {
		root := t.TempDir()
		if _, err := NewLayout(filepath.Join(root, "state"), root, "project"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("formal root overlap accepted: %v", err)
		}
	})
}
