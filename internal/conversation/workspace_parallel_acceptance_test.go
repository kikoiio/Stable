package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestConcurrentWorkspaceCandidateAcceptsDoNotLoseACompletedSwap(t *testing.T) {
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
	session, err := sessionlog.Create(root, "workspace-concurrent-acceptance")
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
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: session.ID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Authority: permission.Authority{
			RunID: "originrun", SessionID: session.ID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	candidateIDs := make([]string, 0, 2)
	for _, item := range []struct{ label, path, content string }{
		{"first writer", "first.txt", "first accepted change"},
		{"second writer", "second.txt", "second accepted change"},
	} {
		snapshot, err := manager.Create(ctx, scope, item.label)
		if err != nil {
			t.Fatal(err)
		}
		paths, err := layout.Paths(snapshot.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.Checkout, item.path), []byte(item.content), 0600); err != nil {
			t.Fatal(err)
		}
		exported, err := manager.Export(ctx, scope, snapshot.ID)
		if err != nil || exported.CandidateID == "" {
			t.Fatalf("export %s: %+v err=%v", item.label, exported, err)
		}
		candidateIDs = append(candidateIDs, exported.CandidateID)
	}

	decisions := make([]candidate.AcceptanceDecision, 2)
	items := make([]candidate.Candidate, 2)
	reviews := make([]candidate.Review, 2)
	actionIDs := make([]string, 2)
	for i, candidateID := range candidateIDs {
		review, err := service.reviewCandidate(ctx, candidateID, session.ID)
		if err != nil || review.CandidateDigest == "" || review.Digest == "" {
			t.Fatalf("review candidate %s: %+v err=%v", candidateID, review, err)
		}
		record, err := state.GetCandidate(ctx, candidateID)
		if err != nil {
			t.Fatal(err)
		}
		reviews[i] = review
		actionIDs[i] = record.ActionID
		decisionID, err := workspace.NewID()
		if err != nil {
			t.Fatal(err)
		}
		decisions[i] = candidate.AcceptanceDecision{
			ID: decisionID, UserID: "test-user", CandidateID: candidateID,
			CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest,
			FormalDigest: review.FormalDigest, Mode: candidate.AcceptNormal,
		}
		items[i] = record.Candidate
	}

	type acceptanceResult struct {
		index int
		err   error
	}
	results := make(chan acceptanceResult, 2)
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	for i := range decisions {
		go func(i int) {
			ready <- struct{}{}
			<-start
			_, err := candidate.AcceptCandidate(ctx, items[i], reviews[i], decisions[i], "session-"+session.ID, actionIDs[i], state, time.Now().UTC())
			results <- acceptanceResult{index: i, err: err}
		}(i)
	}
	<-ready
	<-ready
	close(start)
	errorsSeen := make([]error, 2)
	for range 2 {
		result := <-results
		errorsSeen[result.index] = result.err
	}
	successes := 0
	for _, err := range errorsSeen {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent accept successes=%d errors=%v; want exactly one serialized acceptance", successes, errorsSeen)
	}
	first, firstErr := os.ReadFile(filepath.Join(formal, "first.txt"))
	second, secondErr := os.ReadFile(filepath.Join(formal, "second.txt"))
	if firstErr != nil || secondErr != nil {
		t.Fatalf("formal project became unreadable after concurrent accept: first=%v second=%v", firstErr, secondErr)
	}
	firstChanged := string(first) == "first accepted change"
	secondChanged := string(second) == "second accepted change"
	if firstChanged == secondChanged {
		t.Fatalf("concurrent accepts did not leave exactly one reviewed change: first=%q second=%q", first, second)
	}
	for i, decision := range decisions {
		_, _, hasReceipt, err := state.CheckAcceptance(ctx, decision)
		if err != nil {
			t.Fatal(err)
		}
		if hasReceipt != (errorsSeen[i] == nil) {
			t.Fatalf("candidate %s receipt=%v disagrees with acceptance error %v", candidateIDs[i], hasReceipt, errorsSeen[i])
		}
	}
}
