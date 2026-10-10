package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type workspaceConflictBindingFixture struct {
	manager *workspace.LifecycleService
	scope   workspace.Scope
	preview workspace.Snapshot
	paths   workspace.Paths
	formal  string
}

func newWorkspaceConflictBindingFixture(t *testing.T) workspaceConflictBindingFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{"alpha.txt": "base alpha", "beta.txt": "base beta"} {
		if err := os.WriteFile(filepath.Join(formal, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "workspace conflict binding")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "resbinding")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{
		Exporter: workspaceCandidateExporter{service: service},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	scope := workspace.Scope{
		ProjectID: "resbinding", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "setup-run", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "unused-candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "three-way conflict digest binding")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(formal, "alpha.txt"): "formal alpha", filepath.Join(formal, "beta.txt"): "formal beta", filepath.Join(paths.Checkout, "alpha.txt"): "workspace alpha", filepath.Join(paths.Checkout, "beta.txt"): "workspace beta"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha.txt", "beta.txt"}; !reflect.DeepEqual(preview.Conflicts, want) || preview.ConflictCount != len(want) {
		t.Fatalf("preview conflicts=%v count=%d; want exact paths %v", preview.Conflicts, preview.ConflictCount, want)
	}
	for name, path := range map[string]string{"baseline": paths.Baseline, "formal": formal, "workspace": paths.Checkout} {
		manifest, err := workspace.BuildManifest(ctx, path, workspace.DefaultLimits())
		if err != nil {
			t.Fatalf("build %s manifest: %v", name, err)
		}
		var got string
		switch name {
		case "baseline":
			got = preview.BaselineDigest
		case "formal":
			got = preview.FormalDigest
		case "workspace":
			got = preview.WorkspaceDigest
		}
		if got == "" || got != manifest.Digest {
			t.Fatalf("preview %s digest=%q; actual=%q", name, got, manifest.Digest)
		}
	}
	return workspaceConflictBindingFixture{manager: manager, scope: scope, preview: preview, paths: paths, formal: formal}
}

func TestWorkspaceConflictResolutionBindsAllSourceDigestsAndExactPaths(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{"baseline", "formal", "workspace"} {
		t.Run("changed_"+source, func(t *testing.T) {
			f := newWorkspaceConflictBindingFixture(t)
			path := filepath.Join(f.formal, "alpha.txt")
			switch source {
			case "baseline":
				path = filepath.Join(f.paths.Baseline, "alpha.txt")
			case "workspace":
				path = filepath.Join(f.paths.Checkout, "alpha.txt")
			}
			if err := os.WriteFile(path, []byte("changed after preview"), 0600); err != nil {
				t.Fatal(err)
			}
			before := captureWorkspaceResolutionSources(t, f)
			_, err := f.manager.ResolveUser(ctx, f.scope, f.preview.ID, "user-a", f.preview.PreviewID, f.preview.Generation, map[string]string{
				"alpha.txt": workspace.UseWorkspace,
				"beta.txt":  workspace.UseFormal,
			})
			if !errors.Is(err, workspace.ErrSourceChanged) {
				t.Fatalf("resolution after %s mutation returned %v; want ErrSourceChanged", source, err)
			}
			after := captureWorkspaceResolutionSources(t, f)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected %s mutation changed B/F/W sources: before=%v after=%v", source, before, after)
			}
			current, err := f.manager.Get(ctx, f.scope, f.preview.ID)
			if err != nil || current.PreviewID != f.preview.PreviewID || current.ResolutionID != "" || current.ResolvedCount != 0 {
				t.Fatalf("rejected stale resolution changed persisted choice: snapshot=%+v err=%v", current, err)
			}
		})
	}

	t.Run("extra_conflict_path", func(t *testing.T) {
		f := newWorkspaceConflictBindingFixture(t)
		_, err := f.manager.ResolveUser(ctx, f.scope, f.preview.ID, "user-a", f.preview.PreviewID, f.preview.Generation, map[string]string{
			"alpha.txt":     workspace.UseWorkspace,
			"beta.txt":      workspace.UseFormal,
			"unrelated.txt": workspace.UseWorkspace,
		})
		if err == nil {
			t.Fatal("resolution accepted a path outside the exact conflict set")
		}
		assertWorkspaceResolutionSourcesUnchanged(t, f)
		current, err := f.manager.Get(ctx, f.scope, f.preview.ID)
		if err != nil || current.ResolutionID != "" || current.ResolvedCount != 0 {
			t.Fatalf("rejected extra path persisted a resolution: snapshot=%+v err=%v", current, err)
		}
	})

	t.Run("missing_conflict_choice", func(t *testing.T) {
		f := newWorkspaceConflictBindingFixture(t)
		// ResolveUser intentionally supports paged choices. Export is the
		// completion boundary and must reject until every preview conflict has
		// exactly one choice.
		if _, err := f.manager.ResolveUser(ctx, f.scope, f.preview.ID, "user-a", f.preview.PreviewID, f.preview.Generation, map[string]string{
			"alpha.txt": workspace.UseWorkspace,
		}); err != nil {
			t.Fatalf("save first page of user choices: %v", err)
		}
		if _, err := f.manager.Export(ctx, f.scope, f.preview.ID); err == nil {
			t.Fatal("export accepted an incomplete choice set")
		}
		assertWorkspaceResolutionSourcesUnchanged(t, f)
		current, err := f.manager.Get(ctx, f.scope, f.preview.ID)
		if err != nil || current.CandidateID != "" || current.State != workspace.StateKept || current.ResolvedCount != 1 {
			t.Fatalf("incomplete resolution produced candidate or unexpected state: snapshot=%+v err=%v", current, err)
		}
	})
}

func captureWorkspaceResolutionSources(t *testing.T, f workspaceConflictBindingFixture) map[string]string {
	t.Helper()
	result := make(map[string]string, 3)
	for name, root := range map[string]string{"baseline": f.paths.Baseline, "formal": f.formal, "workspace": f.paths.Checkout} {
		data, err := os.ReadFile(filepath.Join(root, "alpha.txt"))
		if err != nil {
			t.Fatalf("read %s source: %v", name, err)
		}
		result[name] = string(data)
	}
	return result
}

func assertWorkspaceResolutionSourcesUnchanged(t *testing.T, f workspaceConflictBindingFixture) {
	t.Helper()
	if got := captureWorkspaceResolutionSources(t, f); got["baseline"] != "base alpha" || got["formal"] != "formal alpha" || got["workspace"] != "workspace alpha" {
		t.Fatalf("rejected resolution changed formal/baseline/workspace sources: %v", got)
	}
}
