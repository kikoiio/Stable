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

func TestSequentialWorkspaceCandidateAcceptsPreserveIndependentChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"first.txt": "first baseline", "second.txt": "second baseline"} {
		if err := os.WriteFile(filepath.Join(formal, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "workspace-parallel-acceptance")
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
		if err := manager.Close(ctx); err != nil {
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

	first, err := manager.Create(ctx, scope, "first writer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(ctx, scope, "second writer")
	if err != nil {
		t.Fatal(err)
	}
	firstPaths, err := layout.Paths(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondPaths, err := layout.Paths(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstChange := filepath.Join(firstPaths.Checkout, "first.txt")
	secondChange := filepath.Join(secondPaths.Checkout, "second.txt")
	if err := os.WriteFile(firstChange, []byte("first accepted change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondChange, []byte("second accepted change"), 0600); err != nil {
		t.Fatal(err)
	}

	firstExport, err := manager.Export(ctx, scope, first.ID)
	if err != nil || firstExport.CandidateID == "" {
		t.Fatalf("first workspace export=%+v err=%v", firstExport, err)
	}
	firstCandidate, err := state.GetCandidate(ctx, firstExport.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	accept := func(candidateID string) {
		t.Helper()
		review, err := service.reviewCandidate(ctx, candidateID, session.ID)
		if err != nil || review.CandidateDigest == "" || review.Digest == "" || len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingPass {
			t.Fatalf("candidate %s review=%+v err=%v", candidateID, review, err)
		}
		decisionID, err := workspace.NewID()
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := service.acceptReviewedCandidate(ctx, ClientMsg{
			CandidateID: candidateID, SessionID: session.ID, DecisionID: decisionID,
			PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest,
			FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal),
		})
		if err != nil || receipt.ID == "" {
			t.Fatalf("candidate %s acceptance receipt=%+v err=%v", candidateID, receipt, err)
		}
	}
	accept(firstExport.CandidateID)

	secondPreview, err := manager.Preview(ctx, scope, second.ID)
	if err != nil || secondPreview.ConflictCount != 0 || len(secondPreview.Conflicts) != 0 {
		t.Fatalf("second workspace preview=%+v err=%v; want no conflicts", secondPreview, err)
	}
	secondExport, err := manager.Export(ctx, scope, second.ID)
	if err != nil || secondExport.CandidateID == "" {
		t.Fatalf("second workspace export=%+v err=%v", secondExport, err)
	}
	if secondExport.CandidateID == firstExport.CandidateID {
		t.Fatalf("second export reused first candidate ID %q", secondExport.CandidateID)
	}
	secondCandidate, err := state.GetCandidate(ctx, secondExport.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"first.txt":  "first accepted change",
		"second.txt": "second accepted change",
	} {
		got, err := os.ReadFile(filepath.Join(secondCandidate.Candidate.CandidateRoot, name))
		if err != nil || string(got) != want {
			t.Fatalf("second candidate %s=%q err=%v; want %q", name, got, err, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(firstCandidate.Candidate.CandidateRoot, "second.txt")); err != nil || string(got) != "second baseline" {
		t.Fatalf("first candidate unexpectedly contains second workspace change: %q err=%v", got, err)
	}
	accept(secondExport.CandidateID)

	for name, want := range map[string]string{
		"first.txt":  "first accepted change",
		"second.txt": "second accepted change",
	} {
		got, err := os.ReadFile(filepath.Join(formal, name))
		if err != nil || string(got) != want {
			t.Fatalf("accepted formal %s=%q err=%v; want %q", name, got, err, want)
		}
	}
}
