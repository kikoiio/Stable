//go:build linux || darwin

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
)

func TestRecoveryRetriesRemovedStateWhenBindingCleanupFails(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "recovery-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}

	newLayout := func() *Layout {
		t.Helper()
		layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		return layout
	}
	layout := newLayout()
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "recover binding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(context.Background(), scope, created.ID); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Snapshot.State = StateRemoving
	record.Snapshot.Cursor++
	record.Operation = Operation{ID: mustID(), Kind: "remove", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := service.store.Save(context.Background(), scope, record, record.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(paths.Root); err != nil {
		t.Fatal(err)
	}

	bindingPath := filepath.Join(layout.projectRoot(), ".bindings", scope.SessionID+".json")
	bindingBytes, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bindingPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindingPath, "preserve"), []byte("external entry"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService(newLayout(), Limits{}, ServiceDependencies{}); err == nil {
		t.Fatal("startup accepted a binding directory where the owned binding file should be")
	}
	// Repair the externally replaced binding entry. Recovery must still have
	// the remove intent so that it can clear the matching binding and finish.
	if err := os.RemoveAll(bindingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bindingPath, bindingBytes, 0600); err != nil {
		t.Fatal(err)
	}
	recoveredLayout := newLayout()
	recovered, err := NewService(recoveredLayout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close(context.Background())
	got, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || got.State != StateRemoved {
		t.Fatalf("recovered state=%s err=%v; want removed", got.State, err)
	}
	if bound, err := recovered.Binding(scope); err != nil || bound != "" {
		t.Fatalf("stale workspace binding after retry=%q err=%v", bound, err)
	}
}
