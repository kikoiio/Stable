package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceDeleteConflictResolutionExportsSelectedSide(t *testing.T) {
	for _, choice := range []string{workspace.UseWorkspace, workspace.UseFormal} {
		t.Run(choice, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			formal := filepath.Join(root, "project")
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			formalFile := filepath.Join(formal, "board.txt")
			if err := os.WriteFile(formalFile, []byte("base"), 0600); err != nil {
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
			socketDir, err := os.MkdirTemp(os.TempDir(), "m09-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "conversation.sock")
			svc, err := Serve(ctx, Deps{
				Store: db, ProjectRoot: formal,
				WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
				SocketPath:         socket, PollEvery: time.Hour,
				CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := svc.Close(); err != nil {
					t.Errorf("close conversation service: %v", err)
				}
			})
			reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			sessions, err := Request(reqctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
			if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
				t.Fatalf("create session: messages=%+v err=%v", sessions, err)
			}
			sessionID := sessions[0].Session.ID
			formalAbs, err := filepath.Abs(formal)
			if err != nil {
				t.Fatal(err)
			}
			_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: sessionID})
			if err != nil {
				t.Fatal(err)
			}
			scope.Authority = permission.Authority{
				RunID: "fixture-run", SessionID: sessionID,
				AllowedRoot: formalAbs, FormalRoot: formalAbs,
				CandidateRoot: filepath.Join(root, "candidate"),
			}
			manager, err := svc.workspaceService(formal)
			if err != nil {
				t.Fatal(err)
			}
			created, err := manager.Create(ctx, scope, "delete conflict export")
			if err != nil {
				t.Fatal(err)
			}
			layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			defer layout.Close()
			paths, err := layout.Paths(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			baselineFile := filepath.Join(paths.Baseline, "board.txt")
			if got, err := os.ReadFile(baselineFile); err != nil || string(got) != "base" {
				t.Fatalf("initial baseline=%q err=%v", got, err)
			}
			if err := os.WriteFile(formalFile, []byte("formal"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(paths.Checkout, "board.txt")); err != nil {
				t.Fatal(err)
			}

			previewMessages, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID})
			if err != nil || len(previewMessages) != 1 || previewMessages[0].Worktree == nil {
				t.Fatalf("preview delete/modify conflict: messages=%+v err=%v", previewMessages, err)
			}
			preview := previewMessages[0].Worktree
			if preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
				t.Fatalf("unexpected delete/modify preview: %+v", preview)
			}
			resolved, err := Request(reqctx, socket, ClientMsg{
				Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
				WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
				ConflictChoices: map[string]string{"board.txt": choice},
			})
			if err != nil || len(resolved) != 1 || resolved[0].Worktree == nil || resolved[0].Worktree.ResolvedCount != 1 {
				t.Fatalf("resolve delete/modify conflict: messages=%+v err=%v", resolved, err)
			}
			exported, err := Request(reqctx, socket, ClientMsg{Op: "worktree_export", SessionID: sessionID, ID: created.ID})
			if err != nil || len(exported) != 1 || exported[0].Worktree == nil || exported[0].Worktree.CandidateID == "" {
				t.Fatalf("export selected conflict side: messages=%+v err=%v", exported, err)
			}
			candidateID := exported[0].Worktree.CandidateID
			candidateRecord, err := db.GetCandidate(ctx, candidateID)
			if err != nil || candidateRecord.Candidate.Status != "ready" {
				t.Fatalf("exported candidate=%+v err=%v; want ready", candidateRecord, err)
			}
			candidateFile := filepath.Join(candidateRecord.Candidate.CandidateRoot, "board.txt")
			if choice == workspace.UseWorkspace {
				if _, err := os.Lstat(candidateFile); !os.IsNotExist(err) {
					t.Fatalf("workspace deletion did not remove candidate path: lstat err=%v", err)
				}
			} else if got, err := os.ReadFile(candidateFile); err != nil || string(got) != "formal" {
				t.Fatalf("formal choice candidate bytes=%q err=%v; want formal", got, err)
			}
			if got, err := os.ReadFile(formalFile); err != nil || string(got) != "formal" {
				t.Fatalf("export changed formal bytes: %q err=%v", got, err)
			}
			if got, err := os.ReadFile(baselineFile); err != nil || string(got) != "base" {
				t.Fatalf("export changed baseline bytes: %q err=%v", got, err)
			}
			review, err := svc.reviewCandidate(ctx, candidateID, sessionID)
			if err != nil || review.CandidateDigest == "" || review.Digest == "" {
				t.Fatalf("exported candidate was not reviewable: review=%+v err=%v", review, err)
			}
			candidateRecord, err = db.GetCandidate(ctx, candidateID)
			if err != nil || candidateRecord.Candidate.Status != "reviewed" {
				t.Fatalf("reviewed candidate=%+v err=%v; want reviewed", candidateRecord, err)
			}
		})
	}
}
