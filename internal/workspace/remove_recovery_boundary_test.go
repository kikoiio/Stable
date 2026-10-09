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
