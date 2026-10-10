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

type resolutionBindingExporter struct{}

func (resolutionBindingExporter) PreviewWorkspace(ctx context.Context, _ Scope, record Record, paths Paths) (Snapshot, error) {
	preview, err := BuildMergePreview(ctx, paths, Limits{})
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		ID: record.Snapshot.ID, BaselineDigest: preview.BaselineDigest,
		FormalDigest: preview.FormalDigest, WorkspaceDigest: preview.WorkspaceDigest,
		ConflictCount: len(preview.Conflicts),
	}
	for _, conflict := range preview.Conflicts {
		snapshot.Conflicts = append(snapshot.Conflicts, conflict.Path)
	}
	return snapshot, nil
}

func (resolutionBindingExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	return Snapshot{}, ErrUnavailable
}

func TestConflictResolutionChoicesAreBoundToFirstUser(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "shared.txt")
	if err := os.WriteFile(formalFile, []byte("base"), 0600); err != nil {
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
	service, err := NewService(layout, Limits{}, ServiceDependencies{IdleGuard: idleWorkspaceGuard{}, Exporter: resolutionBindingExporter{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(ctx); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	created, err := service.Create(ctx, scope, "user-bound resolution")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "shared.txt"), []byte("workspace version"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal version"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "shared.txt" {
		t.Fatalf("conflict preview=%+v err=%v", preview, err)
	}

	first, err := service.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"shared.txt": UseWorkspace})
	if err != nil || first.ResolvedCount != 1 || first.ResolutionID == "" {
		t.Fatalf("first user's resolution=%+v err=%v", first, err)
	}
	if _, err := service.ResolveUser(ctx, scope, created.ID, "user-b", preview.PreviewID, preview.Generation, map[string]string{"shared.txt": UseFormal}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("second user amended another user's resolution: %v", err)
	}
	current, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ResolutionID != first.ResolutionID || current.ResolvedCount != 1 {
		t.Fatalf("rejected second-user action changed the accepted decision: first=%+v current=%+v", first, current)
	}
	// The original decision owner may revise their own choice while the same
	// immutable preview is still current.
	revised, err := service.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"shared.txt": UseFormal})
	if err != nil || revised.ResolvedCount != 1 || revised.ResolutionID == first.ResolutionID {
		t.Fatalf("decision owner could not revise their choice: revised=%+v err=%v", revised, err)
	}
}
