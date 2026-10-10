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

func TestCreateReceiptReadyRecoveryFinalizesWithoutRematerializing(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "create-receipt-recovery-run", SessionID: scope.SessionID,
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
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	creating, err := service.store.Create(ctx, scope, id, "receipt recovery", operationID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := service.layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	gitState, err := service.git.Materialize(ctx, scope, id)
	if err != nil {
		t.Fatalf("materialize complete private workspace before ready journal cut: %v", err)
	}
	rootBefore, err := os.Lstat(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := os.Lstat(filepath.Join(paths.Root, gitStateName))
	if err != nil {
		t.Fatal(err)
	}
	repositoryBefore, err := os.Lstat(paths.Repository)
	if err != nil {
		t.Fatal(err)
	}
	checkoutBefore, err := os.Lstat(paths.Checkout)
	if err != nil {
		t.Fatal(err)
	}
	if creating.Snapshot.State != StateCreating || creating.Operation.Phase != "intent" {
		t.Fatalf("fixture did not leave the durable create intent uncommitted: %+v", creating)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	recovered := open()
	got, err := recovered.Get(ctx, scope, id)
	if err != nil || got.State != StateReady {
		t.Fatalf("receipt recovery state=%+v err=%v, want ready", got, err)
	}
	record, err := recovered.store.Load(ctx, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Operation.ID != operationID || record.Operation.Phase != "complete" ||
		record.Snapshot.BaselineDigest != gitState.BaselineDigest || record.Snapshot.WorkspaceDigest != gitState.BaselineDigest ||
		record.UsedBytes != gitState.UsedBytes {
		t.Fatalf("recovered create journal=%+v, receipt=%+v", record, gitState)
	}
	if same, err := recovered.CreateWithID(ctx, scope, "receipt recovery", id); err != nil || same.ID != id || same.State != StateReady {
		t.Fatalf("idempotent CreateWithID after recovery=%+v err=%v", same, err)
	}
	for name, before := range map[string]os.FileInfo{"root": rootBefore, "receipt": receiptBefore, "repository": repositoryBefore, "checkout": checkoutBefore} {
		path := map[string]string{"root": paths.Root, "receipt": filepath.Join(paths.Root, gitStateName), "repository": paths.Repository, "checkout": paths.Checkout}[name]
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("recovery replaced materialized %s: before=%v after=%v err=%v", name, before, after, err)
		}
	}
	firstCursor := got.Cursor
	if err := recovered.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second := open()
	defer second.Close(ctx)
	repeated, err := second.Get(ctx, scope, id)
	finalRecord, loadErr := second.store.Load(ctx, scope, id)
	if err != nil || loadErr != nil || repeated.State != StateReady || repeated.Cursor != firstCursor || finalRecord.Operation.ID != operationID || finalRecord.Operation.Phase != "complete" {
		t.Fatalf("second receipt recovery changed finalized create: snapshot=%+v record=%+v err=%v loadErr=%v", repeated, finalRecord, err, loadErr)
	}
}

func TestCreateReceiptRecoveryWithoutValidReceiptFailsClosedAndRetainsData(t *testing.T) {
	for _, missing := range []bool{true, false} {
		name := "replaced-receipt"
		if missing {
			name = "missing-receipt"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			parent := t.TempDir()
			formal := filepath.Join(parent, "formal")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			stateRoot := filepath.Join(parent, "state")
			scope := testScope()
			scope.Authority = permission.Authority{RunID: "create-receipt-failclosed-run", SessionID: scope.SessionID,
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
			id, err := NewID()
			if err != nil {
				t.Fatal(err)
			}
			operationID, err := NewID()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.store.Create(ctx, scope, id, "fail closed receipt", operationID); err != nil {
				t.Fatal(err)
			}
			paths, err := service.layout.Paths(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.git.Materialize(ctx, scope, id); err != nil {
				t.Fatalf("materialize before removing receipt evidence: %v", err)
			}
			rootBefore, err := os.Lstat(paths.Root)
			if err != nil {
				t.Fatal(err)
			}
			checkoutFile := filepath.Join(paths.Checkout, "baseline.txt")
			checkoutBytes, err := os.ReadFile(checkoutFile)
			if err != nil {
				t.Fatal(err)
			}
			receiptPath := filepath.Join(paths.Root, gitStateName)
			if missing {
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
			} else {
				oldReceipt := receiptPath + ".old"
				if err := os.Rename(receiptPath, oldReceipt); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(receiptPath, []byte("replacement receipt is invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			receiptBytes, receiptErr := os.ReadFile(receiptPath)
			if missing && !errors.Is(receiptErr, os.ErrNotExist) {
				t.Fatalf("receipt unexpectedly present before restart: %v", receiptErr)
			}
			if !missing && (receiptErr != nil || string(receiptBytes) != "replacement receipt is invalid") {
				t.Fatalf("replacement receipt fixture=%q err=%v", receiptBytes, receiptErr)
			}
			if err := service.Close(ctx); err != nil {
				t.Fatal(err)
			}

			recovered := open()
			got, err := recovered.Get(ctx, scope, id)
			if err != nil || got.State == StateReady || got.State != StateInterrupted {
				t.Fatalf("workspace without valid receipt state=%+v err=%v, want interrupted", got, err)
			}
			if _, err := recovered.CreateWithID(ctx, scope, "fail closed receipt", id); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("incomplete create was treated as ready: %v", err)
			}
			rootAfter, err := os.Lstat(paths.Root)
			if err != nil || !os.SameFile(rootBefore, rootAfter) {
				t.Fatalf("fail-closed recovery replaced root: before=%v after=%v err=%v", rootBefore, rootAfter, err)
			}
			if bytes, err := os.ReadFile(checkoutFile); err != nil || string(bytes) != string(checkoutBytes) {
				t.Fatalf("fail-closed recovery changed checkout bytes: got=%q want=%q err=%v", bytes, checkoutBytes, err)
			}
			if missing {
				if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovery recreated absent receipt evidence: %v", err)
				}
			} else if bytes, err := os.ReadFile(receiptPath); err != nil || string(bytes) != "replacement receipt is invalid" {
				t.Fatalf("recovery changed replacement receipt: bytes=%q err=%v", bytes, err)
			}
			if err := recovered.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
