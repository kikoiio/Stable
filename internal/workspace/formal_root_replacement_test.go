//go:build linux || darwin

package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/permission"
	"stable/internal/platform/proc"
)

type formalRootReplacementExporter struct {
	exports  int
	previews int
}

func (e *formalRootReplacementExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	e.exports++
	return Snapshot{ID: "workspace", CandidateID: "candidate"}, nil
}

func (e *formalRootReplacementExporter) PreviewWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	e.previews++
	return Snapshot{ID: "workspace"}, nil
}

func TestWorkspaceWriterRejectsFormalRootReplacementAtSamePath(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("same project bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "lead-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal,
		CandidateRoot: filepath.Join(parent, "candidate"),
	}
	exporter := &formalRootReplacementExporter{}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}, Exporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	serviceClosed := false
	defer func() {
		if !serviceClosed {
			_ = service.Close(ctx)
		}
	}()

	created, err := service.Create(ctx, scope, "root replacement")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := service.AcquireWriter(ctx, scope, created.ID, "writer-before-root-replacement")
	if err != nil {
		t.Fatal(err)
	}

	oldFormal := formal + "-old"
	if err := os.Rename(formal, oldFormal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("same project bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	if got, err := service.Get(ctx, scope, created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("query old workspace after root replacement: snapshot=%+v err=%v", got, err)
	}
	if _, err := service.ReserveWriterWrite(ctx, lease, 0); err == nil {
		t.Fatal("active writer lease remained usable after its formal root was replaced at the same path")
	}
	tracked := proc.TrackedProcess{
		PID: 1, ProcessGroup: 1, StartTimeTicks: 1, Token: strings.Repeat("a", 64),
		WorkspaceID: lease.WorkspaceID, RunID: lease.RunID, Generation: lease.Generation,
	}
	if err := service.RegisterWriterProcess(ctx, lease, tracked); err == nil {
		t.Fatal("command process registration accepted a writer after its formal root was replaced")
	}
	if _, err := service.ReleaseCompletedWriter(ctx, lease); err != nil {
		t.Fatalf("settle active writer after root replacement: %v", err)
	}
	if _, err := service.AcquireWriter(ctx, scope, created.ID, "writer-run"); err == nil {
		t.Fatal("writer lease accepted a workspace after its formal root was replaced at the same path")
	}
	if _, err := service.Preview(ctx, scope, created.ID); err == nil {
		t.Fatal("preview accepted a workspace after its formal root was replaced at the same path")
	}
	if _, err := service.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export accepted a workspace after its formal root was replaced at the same path")
	}
	if exporter.previews != 0 || exporter.exports != 0 {
		t.Fatalf("root replacement reached exporter: previews=%d exports=%d", exporter.previews, exporter.exports)
	}
	if _, err := service.Exit(ctx, scope); err != nil {
		t.Fatalf("release old workspace binding after root replacement: %v", err)
	}
	if _, err := service.Enter(ctx, scope, created.ID); err == nil {
		t.Fatal("re-enter accepted a workspace after its formal root was replaced at the same path")
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	serviceClosed = true

	restartedLayout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(restartedLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	if got, err := restarted.Get(ctx, scope, created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("query persisted old workspace after service restart: snapshot=%+v err=%v", got, err)
	}
	if _, err := restarted.AcquireWriter(ctx, scope, created.ID, "restarted-writer-run"); err == nil {
		t.Fatal("restarted service accepted workspace after formal root replacement at same path")
	}
}

func TestLegacyWorkspaceReceiptAllowsOnlyExplicitUserDiscard(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "legacy-project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.ProjectID = "legacy-project"
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(parent, "candidate")}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	created, err := service.Create(ctx, scope, "legacy receipt")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(paths.Root, gitStateName))
	if err != nil {
		t.Fatal(err)
	}
	var legacy GitState
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Version = 1
	legacy.FormalRootIdentity = RootIdentity{}
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, gitStateName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "source.txt"), []byte("user work to retire"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcquireWriter(ctx, scope, created.ID, "legacy-writer"); err == nil {
		t.Fatal("legacy receipt authorized a new writer")
	}
	preview, err := service.PreviewDiscardUser(ctx, scope, created.ID, "actual-user")
	if err != nil {
		t.Fatalf("preview explicit discard for legacy workspace: %v", err)
	}
	if preview.DiscardID == "" || preview.DiscardDigest == "" {
		t.Fatalf("legacy discard lacks a user-bound digest confirmation: %+v", preview)
	}
	removed, err := service.RemoveDiscardUser(ctx, scope, created.ID, "actual-user", preview.DiscardID, preview.DiscardDigest, preview.Generation)
	if err != nil {
		t.Fatalf("remove legacy workspace after explicit digest confirmation: %v", err)
	}
	if removed.State != StateRemoved {
		t.Fatalf("legacy workspace state=%s, want removed", removed.State)
	}
}

func TestAcceptedFormalRootRebindPersistsAcrossServiceRestart(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	accepted := filepath.Join(parent, "accepted")
	for _, path := range []string{formal, accepted} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("before acceptance"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accepted, "source.txt"), []byte("after acceptance"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "accepted-project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.ProjectID = "accepted-project"
	scope.Authority = permission.Authority{RunID: "lead-run", SessionID: scope.SessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: accepted}
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, scope, "survive accepted root exchange")
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity, err := CaptureRootIdentity(formal)
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, err := CaptureRootIdentity(accepted)
	if err != nil {
		t.Fatal(err)
	}
	oldFormal := formal + "-old"
	if err := os.Rename(formal, oldFormal); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(accepted, formal); err != nil {
		t.Fatal(err)
	}
	if err := service.RebindAcceptedFormalRoot(ctx, oldIdentity, targetIdentity); err != nil {
		t.Fatalf("rebind workspace after accepted root exchange: %v", err)
	}
	lease, err := service.AcquireWriter(ctx, scope, created.ID, "post-acceptance-writer")
	if err != nil {
		t.Fatalf("workspace writer remained blocked after accepted root exchange: %v", err)
	}
	if _, err := service.ReleaseCompletedWriter(ctx, lease); err != nil {
		t.Fatalf("release post-acceptance writer: %v", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	restartedLayout, err := NewLayout(filepath.Join(parent, "state"), formal, "accepted-project")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(restartedLayout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	if _, err := restarted.AcquireWriter(ctx, scope, created.ID, "post-restart-writer"); err != nil {
		t.Fatalf("rebound workspace receipt did not survive service restart: %v", err)
	}
}
