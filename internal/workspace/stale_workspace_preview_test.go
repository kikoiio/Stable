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

type staleWorkspacePreviewExporter struct{}

func (staleWorkspacePreviewExporter) PreviewWorkspace(ctx context.Context, _ Scope, record Record, paths Paths) (Snapshot, error) {
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

func (staleWorkspacePreviewExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	return Snapshot{}, ErrUnavailable
}

func TestWorkspaceResolutionRejectsStaleWorkspaceDigest(t *testing.T) {
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
	service, err := NewService(layout, Limits{}, ServiceDependencies{
		IdleGuard: idleWorkspaceGuard{}, Exporter: staleWorkspacePreviewExporter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(ctx); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})

	created, err := service.Create(ctx, scope, "stale workspace preview")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "shared.txt"), []byte("workspace at preview"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal at preview"), 0600); err != nil {
		t.Fatal(err)
	}

	preview, err := service.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "shared.txt" {
		t.Fatalf("preview=%+v err=%v; want one shared.txt conflict", preview, err)
	}
	baselineDigest, formalDigest := preview.BaselineDigest, preview.FormalDigest
	if err := os.WriteFile(filepath.Join(paths.Checkout, "shared.txt"), []byte("workspace changed after preview"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := service.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"shared.txt": UseWorkspace}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("resolution against stale workspace digest returned %v, want ErrSourceChanged", err)
	}
	current, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PreviewID != preview.PreviewID || current.ResolutionID != "" || current.ResolvedCount != 0 || current.BaselineDigest != baselineDigest || current.FormalDigest != formalDigest {
		t.Fatalf("stale workspace resolution changed preview or persisted a decision: %+v", current)
	}
	formalBytes, err := os.ReadFile(formalFile)
	if err != nil || string(formalBytes) != "formal at preview" {
		t.Fatalf("formal root changed after rejected stale resolution: %q err=%v", formalBytes, err)
	}
}
