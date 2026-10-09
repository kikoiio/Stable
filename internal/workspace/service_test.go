//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
)

type idleWorkspaceGuard struct{ err error }

func (g idleWorkspaceGuard) CanSwitchWorkspace(context.Context, Scope) error { return g.err }
func (g idleWorkspaceGuard) LockWorkspaceBinding(context.Context, Scope) (func(), error) {
	if g.err != nil {
		return nil, g.err
	}
	return func() {}, nil
}

type previewWorkspaceExporter struct{}

func (previewWorkspaceExporter) PreviewWorkspace(_ context.Context, _ Scope, record Record, _ Paths) (Snapshot, error) {
	return Snapshot{ID: record.Snapshot.ID, BaselineDigest: strings.Repeat("a", 64), FormalDigest: strings.Repeat("b", 64), WorkspaceDigest: strings.Repeat("c", 64), ConflictCount: 1, Conflicts: []string{"conflict.txt"}}, nil
}

func (previewWorkspaceExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	return Snapshot{}, ErrUnavailable
}

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

func TestLifecycleServiceBindingsAreIndependentPerSession(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("shared project baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	firstScope := testScope()
	firstScope.Authority = permission.Authority{RunID: "run-first", SessionID: firstScope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	secondScope := firstScope
	secondScope.SessionID = "session-second"
	secondScope.Work.SessionID = secondScope.SessionID
	secondScope.Authority.RunID = "run-second"
	secondScope.Authority.SessionID = secondScope.SessionID
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	first, err := service.Create(context.Background(), firstScope, "first session")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), secondScope, "second session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(context.Background(), firstScope, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(context.Background(), secondScope, second.ID); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		scope Scope
		own   Snapshot
		other Snapshot
	}{
		{firstScope, first, second},
		{secondScope, second, first},
	} {
		bound, err := service.Binding(fixture.scope)
		if err != nil || bound != fixture.own.ID {
			t.Fatalf("session %s binding=%q, err=%v; want %q", fixture.scope.SessionID, bound, err, fixture.own.ID)
		}
		if _, err := service.Get(context.Background(), fixture.scope, fixture.other.ID); !errors.Is(err, ErrOwnership) {
			t.Fatalf("session %s read another session's workspace: %v", fixture.scope.SessionID, err)
		}
		if _, err := service.Enter(context.Background(), fixture.scope, fixture.other.ID); !errors.Is(err, ErrOwnership) {
			t.Fatalf("session %s entered another session's workspace: %v", fixture.scope.SessionID, err)
		}
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

func TestWorkspaceWriterLeaseIsExclusiveAndGenerationFenced(t *testing.T) {
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
	created, err := service.Create(context.Background(), scope, "exclusive writer")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.AcquireWriter(context.Background(), scope, created.ID, "child-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcquireWriter(context.Background(), scope, created.ID, "child-run-two"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("second writer acquired an active workspace: %v", err)
	}
	active, err := service.Get(context.Background(), scope, created.ID)
	if err != nil || active.State != StateWriting || active.WriterRunID != first.RunID || active.Generation != first.Generation {
		t.Fatalf("rejected writer changed active lease: snapshot=%+v err=%v; first=%+v", active, err, first)
	}
	if _, err := service.ReleaseCompletedWriter(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second, err := service.AcquireWriter(context.Background(), scope, created.ID, "child-run-two")
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("writer generation did not advance: first=%d second=%d", first.Generation, second.Generation)
	}
	if _, err := service.ReserveWriterWrite(context.Background(), first, 1); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale writer reserved a write under the next lease: %v", err)
	}
	if _, err := service.ReleaseCompletedWriter(context.Background(), first); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale writer released the next lease: %v", err)
	}
	current, err := service.Get(context.Background(), scope, created.ID)
	if err != nil || current.State != StateWriting || current.WriterRunID != second.RunID || current.Generation != second.Generation {
		t.Fatalf("stale lease changed current writer: snapshot=%+v err=%v; second=%+v", current, err, second)
	}
}

func TestIndependentWorkspacesHoldConcurrentWriterLeases(t *testing.T) {
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

	firstWorkspace, err := service.Create(context.Background(), scope, "parallel writer one")
	if err != nil {
		t.Fatal(err)
	}
	secondWorkspace, err := service.Create(context.Background(), scope, "parallel writer two")
	if err != nil {
		t.Fatal(err)
	}
	firstRunID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	secondRunID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.AcquireWriter(context.Background(), scope, firstWorkspace.ID, firstRunID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.AcquireWriter(context.Background(), scope, secondWorkspace.ID, secondRunID)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkspaceID == second.WorkspaceID || first.RunID == second.RunID || first.Paths.Checkout == second.Paths.Checkout {
		t.Fatalf("independent leases share identity or checkout: first=%+v second=%+v", first, second)
	}
	for _, lease := range []WriterLease{first, second} {
		active, err := service.Get(context.Background(), scope, lease.WorkspaceID)
		if err != nil || active.State != StateWriting || active.WriterRunID != lease.RunID || active.Generation != lease.Generation {
			t.Fatalf("concurrent writer was not retained: snapshot=%+v err=%v lease=%+v", active, err, lease)
		}
		duplicateRunID, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.AcquireWriter(context.Background(), scope, lease.WorkspaceID, duplicateRunID); !errors.Is(err, ErrOwnership) {
			t.Fatalf("occupied workspace %s accepted a second lease: %v", lease.WorkspaceID, err)
		}
	}

	for _, item := range []struct {
		lease WriterLease
		name  string
		body  string
	}{{first, "first.txt", "writer one"}, {second, "second.txt", "writer two"}} {
		reservation, err := service.ReserveWriterWrite(context.Background(), item.lease, int64(len(item.body)+(64<<10)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(item.lease.Paths.Checkout, item.name), []byte(item.body), 0600); err != nil {
			reservation.Release()
			t.Fatal(err)
		}
		if err := reservation.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, lease := range []WriterLease{first, second} {
		kept, err := service.ReleaseCompletedWriter(context.Background(), lease)
		if err != nil {
			t.Fatal(err)
		}
		if kept.State != StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 1 {
			t.Fatalf("writer completion was not accounted independently: %+v", kept)
		}
	}
}
func TestWorkspacePreviewPersistsBoundedConflictSummary(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{Exporter: previewWorkspaceExporter{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	created, err := service.Create(context.Background(), scope, "preview conflict")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(context.Background(), scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State != StateReady || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "conflict.txt" || preview.BaselineDigest != strings.Repeat("a", 64) || preview.FormalDigest != strings.Repeat("b", 64) || preview.WorkspaceDigest != strings.Repeat("c", 64) {
		t.Fatalf("preview did not expose the bound conflict summary: %+v", preview)
	}
	loaded, err := service.Get(context.Background(), scope, created.ID)
	if err != nil || loaded.Cursor != preview.Cursor || loaded.Conflicts[0] != "conflict.txt" || loaded.FormalDigest != preview.FormalDigest {
		t.Fatalf("preview was not persisted: %+v, %v", loaded, err)
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

func TestUserDiscardRequiresCurrentPreviewDigestAndGeneration(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	created, err := service.Create(context.Background(), scope, "user discard")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(paths.Checkout, "dirty.txt")
	if err := os.WriteFile(dirtyPath, []byte("uncommitted work"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewDiscardUser(context.Background(), scope, created.ID, "user-a")
	if err != nil || preview.DiscardID == "" || preview.DiscardDigest == "" || preview.ChangedFiles == 0 || preview.Generation != created.Generation {
		t.Fatalf("discard preview=%+v err=%v", preview, err)
	}
	if _, err := service.RemoveDiscardUser(context.Background(), scope, created.ID, "user-b", preview.DiscardID, preview.DiscardDigest, preview.Generation); !errors.Is(err, ErrOwnership) {
		t.Fatalf("another user reused discard preview: %v", err)
	}
	if _, err := service.RemoveDiscardUser(context.Background(), scope, created.ID, "user-a", preview.DiscardID, "stale-digest", preview.Generation); !errors.Is(err, ErrOwnership) {
		t.Fatalf("discard accepted a different digest: %v", err)
	}
	if err := os.WriteFile(dirtyPath, []byte("changed after preview"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemoveDiscardUser(context.Background(), scope, created.ID, "user-a", preview.DiscardID, preview.DiscardDigest, preview.Generation); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("discard accepted content changed after preview: %v", err)
	}
	current, err := service.PreviewDiscardUser(context.Background(), scope, created.ID, "user-a")
	if err != nil || current.DiscardID == preview.DiscardID || current.DiscardDigest == preview.DiscardDigest {
		t.Fatalf("changed content did not produce a fresh discard decision: %+v err=%v", current, err)
	}
	removed, err := service.RemoveDiscardUser(context.Background(), scope, created.ID, "user-a", current.DiscardID, current.DiscardDigest, current.Generation)
	if err != nil || removed.State != StateRemoved {
		t.Fatalf("current user discard result=%+v err=%v", removed, err)
	}
	if _, err := os.Stat(paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed discard retained workspace root: %v", err)
	}
}

func TestRemoveCleanPreservesDirtyUntrackedWorkspace(t *testing.T) {
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{RunID: "run-remove", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())

	created, err := service.Create(context.Background(), scope, "preserve untracked work")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(paths.Checkout, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("user work"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemoveClean(context.Background(), scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("clean remove accepted a dirty workspace: %v", err)
	}
	retained, err := service.Get(context.Background(), scope, created.ID)
	if err != nil || retained.State != StateReady {
		t.Fatalf("dirty workspace state after rejected clean remove=%+v err=%v", retained, err)
	}
	content, err := os.ReadFile(untracked)
	if err != nil || string(content) != "user work" {
		t.Fatalf("rejected clean remove lost untracked data: content=%q err=%v", content, err)
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
	stoppingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	createStoppingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	stopping, err := store.Create(context.Background(), scope, stoppingID, "stopping", createStoppingID)
	if err != nil {
		t.Fatal(err)
	}
	stopping.Snapshot.State = StateReady
	stopping.Snapshot.Cursor++
	stopping.Operation = Operation{ID: createStoppingID, Kind: "create", Phase: "complete", Generation: stopping.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, stopping, stopping.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	stopping.Snapshot.State = StateWriting
	stopping.Snapshot.WriterRunID = "stopping-run"
	stopping.Snapshot.Generation++
	stopping.Snapshot.Cursor++
	writerOperationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	stopping.Operation = Operation{ID: writerOperationID, Kind: "writer", Phase: "complete", Generation: stopping.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, stopping, stopping.Snapshot.Generation-1); err != nil {
		t.Fatal(err)
	}
	stopping.Snapshot.State = StateStopping
	stopping.Snapshot.Generation++
	stopping.Snapshot.Cursor++
	stopOperationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	stopping.Operation = Operation{ID: stopOperationID, Kind: "stop", Phase: "intent", Generation: stopping.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, stopping, stopping.Snapshot.Generation-1); err != nil {
		t.Fatal(err)
	}
	exportingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	createExportingID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	exporting, err := store.Create(context.Background(), scope, exportingID, "exporting", createExportingID)
	if err != nil {
		t.Fatal(err)
	}
	exporting.Snapshot.State = StateReady
	exporting.Snapshot.Cursor++
	exporting.Operation = Operation{ID: createExportingID, Kind: "create", Phase: "complete", Generation: exporting.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, exporting, exporting.Snapshot.Generation); err != nil {
		t.Fatal(err)
	}
	candidateID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	exporting.Snapshot.State = StateExporting
	exporting.Snapshot.CandidateID = candidateID
	exporting.Snapshot.Cursor++
	exportOperation, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	exporting.Operation = Operation{ID: exportOperation, Kind: "export", Phase: "intent", Generation: exporting.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := store.Save(context.Background(), scope, exporting, exporting.Snapshot.Generation); err != nil {
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
	gotStopping, err := service.Get(context.Background(), scope, stoppingID)
	if err != nil || gotStopping.State != StateInterrupted || gotStopping.WriterRunID != "stopping-run" {
		t.Fatalf("stopping recovery state=%s writer=%q err=%v", gotStopping.State, gotStopping.WriterRunID, err)
	}
	if _, err := service.StopWriter(context.Background(), scope, stoppingID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("recovery claimed that stopping writer exited: %v", err)
	}
	gotExporting, err := service.Get(context.Background(), scope, exportingID)
	if err != nil || gotExporting.State != StateInterrupted || gotExporting.CandidateID != candidateID || !strings.Contains(gotExporting.Error, "during export") {
		t.Fatalf("export recovery state=%s candidate=%q error=%q err=%v", gotExporting.State, gotExporting.CandidateID, gotExporting.Error, err)
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
	gotStopping, err = service.Get(context.Background(), scope, stoppingID)
	if err != nil || gotStopping.State != StateInterrupted || gotStopping.WriterRunID != "stopping-run" {
		t.Fatalf("repeated stopping recovery state=%s writer=%q err=%v", gotStopping.State, gotStopping.WriterRunID, err)
	}
	gotExporting, err = service.Get(context.Background(), scope, exportingID)
	if err != nil || gotExporting.State != StateInterrupted || gotExporting.CandidateID != candidateID || !strings.Contains(gotExporting.Error, "during export") {
		t.Fatalf("repeated export recovery state=%s candidate=%q error=%q err=%v", gotExporting.State, gotExporting.CandidateID, gotExporting.Error, err)
	}
}
