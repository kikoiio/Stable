//go:build linux || darwin

package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
)

type paginatedConflictExporter struct{}

func (paginatedConflictExporter) PreviewWorkspace(ctx context.Context, _ Scope, record Record, paths Paths) (Snapshot, error) {
	preview, err := BuildMergePreview(ctx, paths, Limits{})
	if err != nil {
		return Snapshot{}, err
	}
	conflicts := make([]string, 0, min(100, len(preview.Conflicts)))
	for _, conflict := range preview.Conflicts {
		if len(conflicts) == cap(conflicts) {
			break
		}
		conflicts = append(conflicts, conflict.Path)
	}
	return Snapshot{
		ID: record.Snapshot.ID, BaselineDigest: preview.BaselineDigest,
		FormalDigest: preview.FormalDigest, WorkspaceDigest: preview.WorkspaceDigest,
		ConflictCount: len(preview.Conflicts), Conflicts: conflicts,
	}, nil
}

func (paginatedConflictExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	return Snapshot{}, ErrUnavailable
}

func TestConflictResolutionPagesAccumulateEveryExactPath(t *testing.T) {
	ctx := context.Background()
	const conflictCount = 205
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < conflictCount; i++ {
		name := fmt.Sprintf("conflict-%03d.txt", i)
		if err := os.WriteFile(filepath.Join(formal, name), []byte("baseline"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	layout, err := NewLayout(filepath.Join(root, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "lead-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	service, err := NewService(layout, Limits{}, ServiceDependencies{
		IdleGuard: idleWorkspaceGuard{}, Exporter: paginatedConflictExporter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(ctx); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	created, err := service.Create(ctx, scope, "conflict pagination")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < conflictCount; i++ {
		name := fmt.Sprintf("conflict-%03d.txt", i)
		if err := os.WriteFile(filepath.Join(formal, name), []byte("formal"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.Checkout, name), []byte("workspace"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := service.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != conflictCount || len(preview.Conflicts) != 100 || preview.ConflictNext != "conflict-099.txt" {
		t.Fatalf("first conflict page=%+v err=%v", preview, err)
	}
	seen := make(map[string]bool, conflictCount)
	page := preview
	for {
		choices := make(map[string]string, len(page.Conflicts))
		for _, path := range page.Conflicts {
			if seen[path] {
				t.Fatalf("conflict path repeated across pages: %q", path)
			}
			seen[path] = true
			choices[path] = UseWorkspace
		}
		if len(choices) == 0 {
			t.Fatal("conflict page was empty before all paths were resolved")
		}
		if _, err := service.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, choices); err != nil {
			t.Fatalf("resolve page ending at %q: %v", page.Conflicts[len(page.Conflicts)-1], err)
		}
		if page.ConflictNext == "" {
			break
		}
		page, err = service.ConflictPage(ctx, scope, created.ID, page.ConflictNext)
		if err != nil {
			t.Fatalf("read conflicts after %q: %v", preview.ConflictNext, err)
		}
	}
	if len(seen) != conflictCount {
		t.Fatalf("resolved %d unique conflict paths, want %d", len(seen), conflictCount)
	}
	for i := 0; i < conflictCount; i++ {
		name := fmt.Sprintf("conflict-%03d.txt", i)
		if !seen[name] {
			t.Fatalf("conflict path omitted from paged resolution: %q", name)
		}
	}
	current, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ResolvedCount != conflictCount || current.ResolutionID == "" {
		t.Fatalf("paged resolution snapshot=%+v; want all %d conflicts resolved", current, conflictCount)
	}
	if _, err := service.ResolveUser(ctx, scope, created.ID, "user-a", preview.PreviewID, preview.Generation, map[string]string{"not-a-conflict.txt": UseWorkspace}); err == nil {
		t.Fatal("resolution accepted a fabricated path after all exact paths were chosen")
	}
	unchanged, err := service.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ResolutionID != current.ResolutionID || unchanged.ResolvedCount != conflictCount {
		t.Fatalf("rejected fabricated path changed resolution: before=%+v after=%+v", current, unchanged)
	}
}
