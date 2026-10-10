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

func TestLifecycleServiceRestartDuringRemoveRetainsExistingWorkspaceIdempotently(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("formal bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "remove-recovery-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}

	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "remove interrupted")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(paths.Checkout, "user.txt")
	if err := os.WriteFile(userFile, []byte("retain through recovery"), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := service.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Snapshot.State = StateRemoving
	record.Snapshot.Cursor++
	operationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	record.Operation = Operation{ID: operationID, Kind: "remove", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := service.store.Save(context.Background(), scope, record, record.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	openService := func() *LifecycleService {
		t.Helper()
		reopened, err := NewLayout(stateRoot, formal, "project")
		if err != nil {
			t.Fatal(err)
		}
		current, err := NewService(reopened, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	firstRestart := openService()
	interrupted, err := firstRestart.Get(context.Background(), scope, created.ID)
	if err != nil || interrupted.State != StateInterrupted || interrupted.Error == "" {
		t.Fatalf("restart did not retain unresolved remove as interrupted: snapshot=%+v err=%v", interrupted, err)
	}
	if _, err := firstRestart.RemoveClean(context.Background(), scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("clean remove resumed without explicit recovery decision: %v", err)
	}
	content, err := os.ReadFile(userFile)
	if err != nil || string(content) != "retain through recovery" {
		t.Fatalf("interrupted remove lost workspace data: content=%q err=%v", content, err)
	}
	firstRecord, err := firstRestart.store.Load(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstCursor, firstOperationID := interrupted.Cursor, firstRecord.Operation.ID
	if err := firstRestart.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	secondRestart := openService()
	defer secondRestart.Close(context.Background())
	repeated, err := secondRestart.Get(context.Background(), scope, created.ID)
	repeatedRecord, loadErr := secondRestart.store.Load(context.Background(), scope, created.ID)
	if err != nil || loadErr != nil || repeated.State != StateInterrupted || repeated.Cursor != firstCursor || repeatedRecord.Operation.ID != firstOperationID {
		t.Fatalf("repeated recovery changed terminal evidence: first=%+v repeated=%+v record=%+v err=%v loadErr=%v", interrupted, repeated, repeatedRecord, err, loadErr)
	}
	content, err = os.ReadFile(userFile)
	if err != nil || string(content) != "retain through recovery" {
		t.Fatalf("repeated recovery lost workspace data: content=%q err=%v", content, err)
	}
}

func TestLifecycleServiceRestartDuringRemoveRejectsReplacementRootAndRetainsIntent(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "remove-replacement-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}

	layout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), scope, "replace root during remove")
	if err != nil {
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
	operationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	record.Operation = Operation{ID: operationID, Kind: "remove", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := service.store.Save(context.Background(), scope, record, record.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Model a replacement at the same workspace ID path after the durable
	// remove intent, as can happen if an external actor swaps the directory.
	originalRoot := paths.Root + ".original"
	if err := os.Rename(paths.Root, originalRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.Root, 0700); err != nil {
		if restoreErr := os.Rename(originalRoot, paths.Root); restoreErr != nil {
			t.Fatalf("create replacement root: %v; restore original root: %v", err, restoreErr)
		}
		t.Fatal(err)
	}
	sentinel := filepath.Join(paths.Root, "replacement-owned-data.txt")
	if err := os.WriteFile(sentinel, []byte("do not adopt or delete"), 0600); err != nil {
		_ = os.RemoveAll(paths.Root)
		_ = os.Rename(originalRoot, paths.Root)
		t.Fatal(err)
	}

	replacementLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(replacementLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}}); !errors.Is(err, ErrOwnership) {
		_ = replacementLayout.Close()
		t.Fatalf("restart adopted a workspace root with a different physical identity: %v", err)
	}
	_ = replacementLayout.Close()
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "do not adopt or delete" {
		t.Fatalf("replacement root data was changed during failed recovery: content=%q err=%v", content, err)
	}

	// The failed recovery must leave the original journal intent untouched.
	checkLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	checkStore, err := NewOwnershipStore(checkLayout)
	if err != nil {
		_ = checkLayout.Close()
		t.Fatal(err)
	}
	retained, err := checkStore.load(created.ID, false)
	if err != nil || retained.Snapshot.State != StateRemoving || retained.Operation.ID != operationID || retained.Operation.Phase != "intent" {
		_ = checkStore.Close()
		_ = checkLayout.Close()
		t.Fatalf("failed recovery changed remove intent: record=%+v err=%v", retained, err)
	}
	if err := checkStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkLayout.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(paths.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalRoot, paths.Root); err != nil {
		t.Fatal(err)
	}

	restoredLayout, err := NewLayout(stateRoot, formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewService(restoredLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close(context.Background())
	interrupted, err := recovered.Get(context.Background(), scope, created.ID)
	if err != nil || interrupted.State != StateInterrupted {
		t.Fatalf("restored owned root did not recover conservatively: snapshot=%+v err=%v", interrupted, err)
	}
}
