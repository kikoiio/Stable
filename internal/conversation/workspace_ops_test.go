package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type passingWorkspaceChecker struct{}

func (passingWorkspaceChecker) Check(context.Context, candidate.Candidate) (candidate.Finding, error) {
	return candidate.Finding{ID: "workspace-check", Checker: "workspace-test", Result: candidate.FindingPass}, nil
}

func TestInstallMergedManifestKeepsIndependentFormalAndWorkspaceChanges(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	target := filepath.Join(root, "candidate")
	for _, path := range []string{formal, baseline, checkout, target} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, files := range map[string]map[string]string{
		baseline: {"formal.txt": "base", "workspace.txt": "base", "removed.txt": "base"},
		formal:   {"formal.txt": "current", "workspace.txt": "base"},
		checkout: {"formal.txt": "base", "workspace.txt": "worker"},
		target:   {"old.txt": "stale"},
	} {
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	limits := workspace.DefaultLimits()
	base, err := workspace.BuildManifest(context.Background(), baseline, limits)
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := workspace.BuildManifest(context.Background(), formal, limits)
	if err != nil {
		t.Fatal(err)
	}
	workspaceManifest, err := workspace.BuildManifest(context.Background(), checkout, limits)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := workspace.ThreeWayPreview(base, formalManifest, workspaceManifest, limits)
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("merge preview=%+v, err=%v", preview, err)
	}
	if err := installMergedManifest(context.Background(), target, formal, checkout, formalManifest, workspaceManifest, preview.Manifest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"formal.txt": "current", "workspace.txt": "worker"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q, err=%v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "removed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("formal deletion was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale target content survived: %v", err)
	}
}

func TestWorkspacePreviewExporterReturnsOnlyConflictsAndInputDigests(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	for _, path := range []string{formal, baseline, checkout} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{baseline: "base", formal: "formal", checkout: "workspace"} {
		if err := os.WriteFile(filepath.Join(path, "conflict.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scope := workspace.Scope{ProjectID: "project", SessionID: "0123456789abcdef0123456789abcdef", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "0123456789abcdef0123456789abcdef"}}
	record := workspace.Record{Scope: scope, Snapshot: workspace.Snapshot{ID: "1123456789abcdef0123456789abcdef"}}
	preview, err := (workspaceCandidateExporter{service: &Service{}}).PreviewWorkspace(context.Background(), scope, record, workspace.Paths{FormalRoot: formal, Baseline: baseline, Checkout: checkout})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ID != record.Snapshot.ID || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "conflict.txt" || len(preview.BaselineDigest) != 64 || len(preview.FormalDigest) != 64 || len(preview.WorkspaceDigest) != 64 || preview.Summary != "" {
		t.Fatalf("workspace preview included incomplete or unbounded data: %+v", preview)
	}
}

func TestWorkspaceConflictResolutionExportsReviewedCandidateForAcceptance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "workspace-acceptance")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service := &Service{deps: Deps{
		Store: state, ProjectRoot: root,
		CandidateCheckers: []candidate.Checker{passingWorkspaceChecker{}},
	}}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formal, "project1")
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
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "originrun", SessionID: session.ID, AllowedRoot: formalAbs,
		FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidate"),
	}
	created, err := manager.Create(ctx, scope, "workspace conflict")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkoutFile := filepath.Join(paths.Checkout, "board.txt")
	if err := os.WriteFile(checkoutFile, []byte("workspace choice"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal choice"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.ConflictCount != 1 || len(preview.Conflicts) != 1 || preview.Conflicts[0] != "board.txt" {
		t.Fatalf("conflict preview=%+v err=%v", preview, err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export succeeded before a user resolution")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{}); err == nil {
		t.Fatal("empty resolution was accepted")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{"other.txt": workspace.UseWorkspace}); err == nil {
		t.Fatal("resolution of an unrelated path was accepted")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user", "0123456789abcdef0123456789abcdef", preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err == nil {
		t.Fatal("resolution for a stale preview was accepted")
	}
	resolution, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace})
	if err != nil || resolution.ResolvedCount != 1 {
		t.Fatalf("user resolution=%+v err=%v", resolution, err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal changed after resolution"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export accepted a resolution after the formal digest changed")
	}
	preview, err = manager.Preview(ctx, scope, created.ID)
	if err != nil || preview.PreviewID == resolution.PreviewID {
		t.Fatalf("changed inputs did not invalidate preview: preview=%+v err=%v", preview, err)
	}
	if _, err := manager.Export(ctx, scope, created.ID); err == nil {
		t.Fatal("export reused a resolution bound to the prior input digests")
	}
	if _, err := manager.ResolveUser(ctx, scope, created.ID, "user", preview.PreviewID, preview.Generation, map[string]string{"board.txt": workspace.UseWorkspace}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type exportResult struct {
		snapshot workspace.Snapshot
		err      error
	}
	results := make(chan exportResult, 2)
	var exports sync.WaitGroup
	for i := 0; i < 2; i++ {
		exports.Add(1)
		go func() {
			defer exports.Done()
			<-start
			snapshot, err := manager.Export(ctx, scope, created.ID)
			results <- exportResult{snapshot: snapshot, err: err}
		}()
	}
	close(start)
	exports.Wait()
	close(results)
	var exported workspace.Snapshot
	for result := range results {
		if result.err != nil || result.snapshot.State != workspace.StateExported || result.snapshot.CandidateID == "" {
			t.Fatalf("concurrent workspace export=%+v err=%v", result.snapshot, result.err)
		}
		if exported.CandidateID != "" && result.snapshot.CandidateID != exported.CandidateID {
			t.Fatalf("concurrent exports produced different candidates: %q and %q", exported.CandidateID, result.snapshot.CandidateID)
		}
		exported = result.snapshot
	}
	candidateRecord, err := state.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(filepath.Join(candidateRecord.Candidate.CandidateRoot, "board.txt"))
	if err != nil || string(merged) != "workspace choice" {
		t.Fatalf("exported candidate content=%q err=%v", merged, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, session.ID)
	if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("candidate review=%+v err=%v", review, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: exported.CandidateID, SessionID: session.ID, DecisionID: decisionID,
		PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest,
		FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
	})
	if err != nil || receipt.ID == "" {
		t.Fatalf("candidate acceptance receipt=%+v err=%v", receipt, err)
	}
	accepted, err := os.ReadFile(filepath.Join(formal, "board.txt"))
	if err != nil || string(accepted) != "workspace choice" {
		t.Fatalf("accepted formal content=%q err=%v", accepted, err)
	}
}

func TestCanSwitchWorkspaceRequiresIdleOwningSession(t *testing.T) {
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-1"}
	svc := &Service{activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}}
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); err != nil {
		t.Fatalf("idle session cannot switch workspace: %v", err)
	}
	svc.activeRuns["run-1"] = work.SessionID
	svc.activeRequests["run-1"] = agent.ExecutionRequest{RunID: "run-1", Work: work}
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active session run allowed workspace switch: %v", err)
	}
	svc.activeRuns = map[string]string{}
	svc.activeRequests = map[string]agent.ExecutionRequest{}
	tasks := NewAgentTaskCoordinator()
	tasks.active["task-1"] = &agentTaskState{work: work}
	svc.deps.AgentTasks = tasks
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active background task allowed workspace switch: %v", err)
	}
}
