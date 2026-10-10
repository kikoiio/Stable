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

func TestWorkspaceConflictResolutionChoiceMaterializesExactCandidatePath(t *testing.T) {
	for _, tc := range []struct {
		choice string
		want   string
	}{
		{choice: workspace.UseFormal, want: "formal choice bytes"},
		{choice: workspace.UseWorkspace, want: "workspace choice bytes"},
	} {
		t.Run(tc.choice, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			formal := filepath.Join(root, "project")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			formalFile := filepath.Join(formal, "conflict.txt")
			if err := os.WriteFile(formalFile, []byte("baseline bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			session, err := sessionlog.Create(root, "conflict choice materialization")
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := state.Close(); err != nil {
					t.Errorf("close store: %v", err)
				}
			})
			service := &Service{deps: Deps{
				Store: state, ProjectRoot: root,
				CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
			}}
			formalAbs, err := filepath.Abs(formal)
			if err != nil {
				t.Fatal(err)
			}
			layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, "project1")
			if err != nil {
				t.Fatal(err)
			}
			defer layout.Close()
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
				ProjectID: "project1", SessionID: session.ID,
				Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
			}
			scope.Authority = permission.Authority{
				RunID: "choice-materialization-run", SessionID: session.ID,
				AllowedRoot: formalAbs, FormalRoot: formalAbs,
				CandidateRoot: filepath.Join(root, "candidate"),
			}
			created, err := manager.Create(ctx, scope, "resolve exact conflict choice")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			checkoutFile := filepath.Join(paths.Checkout, "conflict.txt")
			if err := os.WriteFile(checkoutFile, []byte("workspace choice bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(formalFile, []byte("formal choice bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			preview, err := manager.Preview(ctx, scope, created.ID)
			if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "conflict.txt" {
				t.Fatalf("conflict preview=%+v err=%v", preview, err)
			}
			resolution, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{"conflict.txt": tc.choice})
			if err != nil || resolution.ResolvedCount != 1 {
				t.Fatalf("resolve %s: result=%+v err=%v", tc.choice, resolution, err)
			}
			exported, err := manager.Export(ctx, scope, created.ID)
			if err != nil || exported.State != workspace.StateExported || exported.CandidateID == "" {
				t.Fatalf("export resolved candidate=%+v err=%v", exported, err)
			}
			record, err := state.GetCandidate(ctx, exported.CandidateID)
			if err != nil || record.Candidate.Status != "ready" {
				t.Fatalf("candidate record=%+v err=%v; want ready", record, err)
			}
			candidateFile := filepath.Join(record.Candidate.CandidateRoot, "conflict.txt")
			got, err := os.ReadFile(candidateFile)
			if err != nil || string(got) != tc.want {
				t.Fatalf("candidate path after %s=%q err=%v; want exact selected bytes %q", tc.choice, got, err, tc.want)
			}
			_, digest, err := candidate.BuildManifestForPolicy(record.Candidate.CandidateRoot, record.Candidate.ManifestPolicy)
			if err != nil || digest != record.Candidate.CandidateDigest {
				t.Fatalf("candidate digest=%q recomputed=%q err=%v", record.Candidate.CandidateDigest, digest, err)
			}
			if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal choice bytes" {
				t.Fatalf("resolution/export changed formal conflict path=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(paths.Baseline, "conflict.txt")); err != nil || string(got) != "baseline bytes" {
				t.Fatalf("resolution/export changed baseline path=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(checkoutFile); err != nil || string(got) != "workspace choice bytes" {
				t.Fatalf("resolution/export changed workspace path=%q err=%v", got, err)
			}
		})
	}
}
