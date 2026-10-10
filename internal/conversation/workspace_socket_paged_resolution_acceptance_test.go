package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceSocketPagedResolutionReviewsAndExplicitlyAccepts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	baselineValues := map[string]string{"alpha.txt": "base alpha", "beta.txt": "base beta"}
	for name, value := range baselineValues {
		if err := os.WriteFile(filepath.Join(formal, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
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
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
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
		RunID: "paged-resolution-origin", SessionID: sessionID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	manager, err := svc.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "paged conflict resolution")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	if err := layout.Close(); err != nil {
		t.Fatal(err)
	}
	workspaceValues := map[string]string{"alpha.txt": "workspace alpha", "beta.txt": "workspace beta"}
	formalValues := map[string]string{"alpha.txt": "formal alpha", "beta.txt": "formal beta"}
	for name, value := range formalValues {
		if err := os.WriteFile(filepath.Join(formal, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range workspaceValues {
		if err := os.WriteFile(filepath.Join(paths.Checkout, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	assertFiles := func(root string, values map[string]string) {
		t.Helper()
		for name, want := range values {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(got) != want {
				t.Fatalf("%s/%s=%q err=%v; want %q", root, name, got, err, want)
			}
		}
	}

	previews, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID})
	if err != nil || len(previews) != 1 || previews[0].Worktree == nil {
		t.Fatalf("preview conflicts: messages=%+v err=%v", previews, err)
	}
	preview := previews[0].Worktree
	wantPaths := []string{"alpha.txt", "beta.txt"}
	if preview.ConflictCount != 2 || !reflect.DeepEqual(preview.Conflicts, wantPaths) || preview.PreviewID == "" || preview.Generation != created.Generation {
		t.Fatalf("initial conflict page=%+v; want two exact paths %v", preview, wantPaths)
	}
	baselineRoot := paths.Baseline
	assertFiles(formal, formalValues)
	assertFiles(baselineRoot, baselineValues)

	first, err := Request(reqctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"alpha.txt": workspace.UseWorkspace},
	})
	if err != nil || len(first) != 1 || first[0].Worktree == nil || first[0].Worktree.ResolvedCount != 1 {
		t.Fatalf("save first user-resolution page: messages=%+v err=%v", first, err)
	}
	page, err := Request(reqctx, socket, ClientMsg{Op: "worktree_preview", SessionID: sessionID, ID: created.ID, ConflictAfter: "alpha.txt"})
	if err != nil || len(page) != 1 || page[0].Worktree == nil || !reflect.DeepEqual(page[0].Worktree.Conflicts, []string{"beta.txt"}) {
		t.Fatalf("fetch second conflict page: messages=%+v err=%v", page, err)
	}
	if page[0].Worktree.PreviewID != preview.PreviewID || page[0].Worktree.Generation != preview.Generation || page[0].Worktree.BaselineDigest != preview.BaselineDigest || page[0].Worktree.FormalDigest != preview.FormalDigest || page[0].Worktree.WorkspaceDigest != preview.WorkspaceDigest {
		t.Fatalf("second page changed preview source identity: first=%+v second=%+v", preview, page[0].Worktree)
	}
	second, err := Request(reqctx, socket, ClientMsg{
		Op: "worktree_resolve", SessionID: sessionID, ID: created.ID,
		WorktreePreviewID: preview.PreviewID, WorktreeGeneration: preview.Generation,
		ConflictChoices: map[string]string{"beta.txt": workspace.UseWorkspace},
	})
	if err != nil || len(second) != 1 || second[0].Worktree == nil || second[0].Worktree.ResolvedCount != 2 {
		t.Fatalf("save second user-resolution page: messages=%+v err=%v", second, err)
	}
	assertFiles(formal, formalValues)
	assertFiles(baselineRoot, baselineValues)

	exported, err := Request(reqctx, socket, ClientMsg{Op: "worktree_export", SessionID: sessionID, ID: created.ID})
	if err != nil || len(exported) != 1 || exported[0].Worktree == nil || exported[0].Worktree.CandidateID == "" {
		t.Fatalf("export paged resolution: messages=%+v err=%v", exported, err)
	}
	candidateID := exported[0].Worktree.CandidateID
	record, err := db.GetCandidate(ctx, candidateID)
	if err != nil || record.Candidate.Status != "ready" || record.Candidate.ManifestPolicy != candidate.ManifestPolicyProject {
		t.Fatalf("exported candidate=%+v err=%v", record.Candidate, err)
	}
	assertFiles(record.Candidate.CandidateRoot, workspaceValues)
	assertFiles(formal, formalValues)
	assertFiles(baselineRoot, baselineValues)
	candidateBaselineDigest := record.Candidate.BaselineDigest
	if candidateBaselineDigest == "" {
		t.Fatal("exported candidate has no baseline digest")
	}
	review, err := svc.reviewCandidate(ctx, candidateID, sessionID)
	if err != nil || review.CandidateDigest == "" || review.FormalDigest == "" || review.Digest == "" || review.CandidateID != candidateID {
		t.Fatalf("candidate review=%+v err=%v", review, err)
	}
	assertFiles(formal, formalValues)
	assertFiles(baselineRoot, baselineValues)
	if record.Candidate.Status == "accepted" {
		t.Fatal("candidate was accepted before the explicit user decision")
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := Request(reqctx, socket, ClientMsg{
		Op: "review_accept", SessionID: sessionID, CandidateID: candidateID,
		DecisionID: decisionID, PreviewDigest: review.Digest,
		CandidateDigest: review.CandidateDigest, FormalDigest: review.FormalDigest,
		AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || len(accepted) != 1 || accepted[0].Type != "acceptance" || accepted[0].Receipt == nil {
		t.Fatalf("explicit review_accept: messages=%+v err=%v", accepted, err)
	}
	receipt := accepted[0].Receipt
	if receipt.ID == "" || receipt.DecisionID != decisionID || receipt.CandidateID != candidateID || receipt.FormalDigest != review.CandidateDigest {
		t.Fatalf("receipt=%+v does not match review=%+v", receipt, review)
	}
	acceptedRecord, err := db.GetCandidate(ctx, candidateID)
	if err != nil || acceptedRecord.Candidate.Status != "accepted" || acceptedRecord.Candidate.BaselineDigest != candidateBaselineDigest || acceptedRecord.Candidate.CandidateDigest != receipt.FormalDigest {
		t.Fatalf("accepted candidate=%+v err=%v", acceptedRecord.Candidate, err)
	}
	assertFiles(formal, workspaceValues)
	assertFiles(baselineRoot, baselineValues)
}
