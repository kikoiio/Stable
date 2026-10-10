//go:build linux || darwin

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
)

func TestKeepAfterRestartIsIdempotentAndRetainsDirtyCheckout(t *testing.T) {
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
	scope.Authority = permission.Authority{RunID: "keep-recovery-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}

	open := func() *LifecycleService {
		t.Helper()
		layout, err := NewLayout(stateRoot, formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
		if err != nil {
			_ = layout.Close()
			t.Fatal(err)
		}
		return service
	}

	service := open()
	created, err := service.Create(ctx, scope, "keep recovery")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := service.layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const retained = "uncommitted user evidence"
	retainedPath := filepath.Join(paths.Checkout, "user-evidence.txt")
	if err := os.WriteFile(retainedPath, []byte(retained), 0600); err != nil {
		t.Fatal(err)
	}

	kept, err := service.Keep(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != StateKept {
		t.Fatalf("Keep state=%q, want %q", kept.State, StateKept)
	}
	beforeClose, err := service.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeClose.Operation.Kind != string(StateKept) || beforeClose.Operation.Phase != "complete" {
		t.Fatalf("Keep did not persist a completed operation: %+v", beforeClose.Operation)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Restart after the durable Keep transition, then repeat it as a retry of a
	// call whose response may have been lost at the crash boundary.
	recovered := open()
	afterRestart, err := recovered.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.Snapshot.State != StateKept || afterRestart.Operation.ID != beforeClose.Operation.ID ||
		afterRestart.Operation.Kind != beforeClose.Operation.Kind || afterRestart.Operation.Phase != "complete" {
		t.Fatalf("restart changed durable Keep identity/state: before=%+v after=%+v", beforeClose, afterRestart)
	}
	stableCursor := afterRestart.Snapshot.Cursor
	stableOperationID := afterRestart.Operation.ID
	if got, err := os.ReadFile(retainedPath); err != nil || string(got) != retained {
		t.Fatalf("restart lost kept checkout evidence: got=%q err=%v", got, err)
	}
	wantUsage, err := DiskUsage(ctx, paths.Root, recovered.limits.Normalized())
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.UsedBytes != wantUsage || afterRestart.UsedBytes <= 0 {
		t.Fatalf("recovered Keep usage=%d, want retained workspace usage %d", afterRestart.UsedBytes, wantUsage)
	}

	retried, err := recovered.Keep(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRetry, err := recovered.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != StateKept || afterRetry.Snapshot.Cursor != stableCursor || afterRetry.Operation.ID != stableOperationID {
		t.Fatalf("repeated Keep was not idempotent: snapshot=%+v record=%+v", retried, afterRetry)
	}
	if got, err := os.ReadFile(retainedPath); err != nil || string(got) != retained {
		t.Fatalf("repeated Keep changed checkout evidence: got=%q err=%v", got, err)
	}
	if err := recovered.Close(ctx); err != nil {
		t.Fatal(err)
	}

	secondRestart := open()
	defer secondRestart.Close(ctx)
	final, err := secondRestart.store.Load(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Snapshot.State != StateKept || final.Snapshot.Cursor != stableCursor || final.Operation.ID != stableOperationID || final.UsedBytes != wantUsage {
		t.Fatalf("second restart changed kept workspace evidence/accounting: %+v", final)
	}
	if got, err := os.ReadFile(retainedPath); err != nil || string(got) != retained {
		t.Fatalf("second restart lost checkout evidence: got=%q err=%v", got, err)
	}
}
