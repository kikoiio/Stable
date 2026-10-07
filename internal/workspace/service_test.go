//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
)

type idleWorkspaceGuard struct{ err error }

func (g idleWorkspaceGuard) CanSwitchWorkspace(context.Context, Scope) error { return g.err }

func TestLifecycleServiceCreatesOwnedPrivateGitWorkspace(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "tracked-and-dirty.txt"), []byte("current project bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	snapshot, err := service.Create(context.Background(), scope, "isolated work")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != StateReady || snapshot.ID == "" || snapshot.BaselineDigest == "" {
		t.Fatalf("unexpected workspace create result: %+v", snapshot)
	}
	paths, err := layout.Paths(snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(paths.Checkout, "tracked-and-dirty.txt"))
	if err != nil || string(content) != "current project bytes" {
		t.Fatalf("workspace snapshot lost project data: %q, %v", content, err)
	}
	if _, err := service.Enter(context.Background(), scope, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if bound, err := service.Binding(scope); err != nil || bound != snapshot.ID {
		t.Fatalf("session binding=%q, %v", bound, err)
	}
	planScope := scope
	planScope.Authority.Mode = permission.ModePlan
	if _, err := service.AcquireWriter(context.Background(), planScope, snapshot.ID, "writer-run"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("plan authority acquired a writer: %v", err)
	}
	if _, err := service.AcquireWriter(context.Background(), scope, snapshot.ID, "writer-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StopWriter(context.Background(), scope, snapshot.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing process stopper released a writer lease: %v", err)
	}
}

func TestWorkspaceWriterAccountsWritesAndReleasesOnlyAfterCompletion(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.OriginRunID = "lead-run"
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	created, err := service.Create(context.Background(), scope, "bounded writer")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(context.Background(), scope, created.ID, "child-run")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := service.ReserveWriterWrite(context.Background(), lease, int64(len("child output")+(64<<10)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Paths.Checkout, "child.txt"), []byte("child output"), 0600); err != nil {
		reservation.Release()
		t.Fatal(err)
	}
	if err := reservation.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	kept, err := service.ReleaseCompletedWriter(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 1 {
		t.Fatalf("writer completion was not durably accounted: %+v", kept)
	}
	if _, err := service.ReserveWriterWrite(context.Background(), lease, 1); !errors.Is(err, ErrOwnership) {
		t.Fatalf("completed lease remained writable: %v", err)
	}
}

func TestWorkspaceScopeMustMatchRunWorkIdentity(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	defer layout.stateIdentity.Close()
	defer layout.projectIdentity.Close()
	service := &LifecycleService{layout: layout}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	scope.Work = agent.WorkRef{Kind: agent.WorkGoal, SessionID: scope.SessionID, GoalID: "goal"}
	if err := service.validateProjectAuthority(scope); !errors.Is(err, ErrOwnership) {
		t.Fatalf("authority for a different work target accepted: %v", err)
	}
}

func TestLifecycleServiceRecoversInterruptedJournalOperationsConservatively(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	defer layout.stateIdentity.Close()
	defer layout.projectIdentity.Close()
	store, err := NewOwnershipStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := testScope()
	creatingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	createOperation, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), scope, creatingID, "creating", createOperation); err != nil {
		t.Fatal(err)
	}
	removingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	removeOperation, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	removing, err := store.Create(context.Background(), scope, removingID, "removing", removeOperation)
	if err != nil {
		t.Fatal(err)
	}
	removing.Snapshot.State = StateRemoving
	removing.Snapshot.Cursor++
	removeIntent, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	removing.Operation = Operation{ID: removeIntent, Kind: "remove", Phase: "intent", Generation: removing.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, removing, removing.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(removingID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(paths.Root); err != nil {
		t.Fatal(err)
	}
	writingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	createWritingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	writing, err := store.Create(context.Background(), scope, writingID, "writing", createWritingID)
	if err != nil {
		t.Fatal(err)
	}
	writing.Snapshot.State = StateReady
	writing.Snapshot.Cursor++
	writing.Operation = Operation{ID: createWritingID, Kind: "create", Phase: "complete", Generation: writing.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, writing, writing.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	writing.Snapshot.State = StateWriting
	writing.Snapshot.WriterRunID = "writer-run"
	writing.Snapshot.Generation++
	writing.Snapshot.Cursor++
	writerOperation, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	writing.Operation = Operation{ID: writerOperation, Kind: "writer", Phase: "complete", Generation: writing.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, writing, writing.Snapshot.Generation-1); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	gotCreating, err := service.Get(context.Background(), scope, creatingID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCreating.State != StateInterrupted {
		t.Fatalf("unfinished create state=%s, want interrupted", gotCreating.State)
	}
	creatingPaths, err := layout.Paths(creatingID)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(creatingPaths.Root); err != nil || len(entries) != 0 {
		t.Fatalf("recovery allocated/adopted partial resource: entries=%v err=%v", entries, err)
	}
	gotRemoving, err := service.Get(context.Background(), scope, removingID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRemoving.State != StateRemoved {
		t.Fatalf("removed root intent state=%s, want removed", gotRemoving.State)
	}
	gotWriting, err := service.Get(context.Background(), scope, writingID)
	if err != nil || gotWriting.State != StateInterrupted || gotWriting.WriterRunID != "writer-run" {
		t.Fatalf("writing recovery state=%s writer=%q err=%v", gotWriting.State, gotWriting.WriterRunID, err)
	}
	if _, err := service.StopWriter(context.Background(), scope, writingID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("recovery claimed to stop an untracked writer: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err = NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatalf("second startup recovery: %v", err)
	}
	defer service.Close(context.Background())
	gotCreating, err = service.Get(context.Background(), scope, creatingID)
	if err != nil || gotCreating.State != StateInterrupted {
		t.Fatalf("repeated recovery state=%s err=%v", gotCreating.State, err)
	}
	gotRemoving, err = service.Get(context.Background(), scope, removingID)
	if err != nil || gotRemoving.State != StateRemoved {
		t.Fatalf("repeated removal recovery state=%s err=%v", gotRemoving.State, err)
	}
	gotWriting, err = service.Get(context.Background(), scope, writingID)
	if err != nil || gotWriting.State != StateInterrupted || gotWriting.WriterRunID != "writer-run" {
		t.Fatalf("repeated writer recovery state=%s writer=%q err=%v", gotWriting.State, gotWriting.WriterRunID, err)
	}
}
