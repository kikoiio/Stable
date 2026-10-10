package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceResolutionExpiresWhenBaselineChangesBeforeExport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "board.txt")
	if err := os.WriteFile(formalFile, []byte("baseline-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace resolution baseline change")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{
		Store: db, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "baselinechange")
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
			t.Errorf("close workspace service: %v", err)
		}
	})
	scope := workspace.Scope{
		ProjectID: "baselinechange", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "setup-run", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "unused-candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "stale baseline resolution")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte("workspace-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formalFile, []byte("formal-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("initial conflict preview=%+v err=%v", preview, err)
	}
	resolution, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace})
	if err != nil || resolution.ResolvedCount != 1 {
		t.Fatalf("initial user resolution=%+v err=%v", resolution, err)
	}

	// The baseline is one of the three source trees bound by the user's choice.
	// Mutating it after resolution must invalidate that choice at export time.
	if err := os.WriteFile(filepath.Join(paths.Baseline, "board.txt"), []byte("baseline-v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); !errors.Is(err, workspace.ErrSourceChanged) {
		t.Fatalf("export after baseline mutation returned %v, want ErrSourceChanged", err)
	}
	if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal-v1" {
		t.Fatalf("failed stale-baseline export changed formal content: content=%q err=%v", got, err)
	}

	if _, err := manager.Preview(ctx, scope, created.ID); !errors.Is(err, workspace.ErrSourceChanged) {
		t.Fatalf("preview after baseline mutation returned %v, want ErrSourceChanged", err)
	}
}
