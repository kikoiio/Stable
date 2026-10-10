package conversation

import (
	"context"
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

func TestWorkspaceCreateConflictResolutionExportsSelectedSide(t *testing.T) {
	for _, choice := range []string{workspace.UseFormal, workspace.UseWorkspace} {
		t.Run(choice, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			formal := filepath.Join(root, "project")
			if err := os.MkdirAll(formal, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("baseline"), 0600); err != nil {
				t.Fatal(err)
			}
			session, err := sessionlog.Create(root, "create conflict resolution")
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("close store: %v", err)
				}
			})
			formalAbs, err := filepath.Abs(formal)
			if err != nil {
				t.Fatal(err)
			}
			scope := workspace.Scope{
				ProjectID: "create-conflict", SessionID: session.ID,
				Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
				Authority: permission.Authority{
					RunID: "create-conflict-origin", SessionID: session.ID,
					AllowedRoot: formalAbs, FormalRoot: formalAbs,
					CandidateRoot: filepath.Join(root, "candidate"),
				},
			}
			layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			service := &Service{deps: Deps{
				Store: db, ProjectRoot: root,
				CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
			}}
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
			created, err := manager.Create(ctx, scope, "conflicting create")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			const path = "new.txt"
			const formalValue = "formal created value"
			const workspaceValue = "workspace created value"
			if err := os.WriteFile(filepath.Join(formal, path), []byte(formalValue), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(paths.Checkout, path), []byte(workspaceValue), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(paths.Baseline, path)); !os.IsNotExist(err) {
				t.Fatalf("baseline unexpectedly contains newly created conflict path: %v", err)
			}

			preview, err := manager.Preview(ctx, scope, created.ID)
			if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != path {
				t.Fatalf("create/create conflict preview=%+v err=%v", preview, err)
			}
			mergePreview, err := workspace.BuildMergePreview(ctx, paths, workspace.DefaultLimits())
			if err != nil || len(mergePreview.Conflicts) != 1 {
				t.Fatalf("build exact create/create merge conflict=%+v err=%v", mergePreview.Conflicts, err)
			}
			conflict := mergePreview.Conflicts[0]
			if conflict.Path != path || conflict.Base != nil || conflict.Formal == nil || conflict.Workspace == nil || conflict.Formal.Digest == conflict.Workspace.Digest {
				t.Fatalf("create/create conflict sides=%+v; want absent B and distinct exact F/W entries", conflict)
			}
			choiceValue := formalValue
			if choice == workspace.UseWorkspace {
				choiceValue = workspaceValue
			}
			resolved, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{path: choice})
			if err != nil || resolved.ResolvedCount != 1 {
				t.Fatalf("resolve created-path conflict with %s: snapshot=%+v err=%v", choice, resolved, err)
			}
			exported, err := manager.Export(ctx, scope, created.ID)
			if err != nil || exported.CandidateID == "" {
				t.Fatalf("export resolved created-path conflict: snapshot=%+v err=%v", exported, err)
			}
			record, err := db.GetCandidate(ctx, exported.CandidateID)
			if err != nil || record.Candidate.Status != "ready" {
				t.Fatalf("created-path candidate=%+v err=%v", record.Candidate, err)
			}
			if got, err := os.ReadFile(filepath.Join(record.Candidate.CandidateRoot, path)); err != nil || string(got) != choiceValue {
				t.Fatalf("candidate %s=%q err=%v; want selected side %q", path, got, err, choiceValue)
			}
			if got, err := os.ReadFile(filepath.Join(formal, path)); err != nil || string(got) != formalValue {
				t.Fatalf("export changed formal created path: %q err=%v", got, err)
			}
			if _, err := os.Lstat(filepath.Join(paths.Baseline, path)); !os.IsNotExist(err) {
				t.Fatalf("export changed baseline path existence: %v", err)
			}
		})
	}
}
