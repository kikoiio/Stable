package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

// acceptanceGateStore pauses acceptance after AcceptCandidate has acquired
// the formal-root transaction lock, but before it validates or mutates roots.
type acceptanceGateStore struct {
	*store.Store
	entered chan struct{}
	release chan struct{}
}

func (s *acceptanceGateStore) CheckAcceptance(ctx context.Context, d candidate.AcceptanceDecision) (bool, candidate.Receipt, bool, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, candidate.Receipt{}, false, ctx.Err()
	}
	return s.Store.CheckAcceptance(ctx, d)
}

func (s *acceptanceGateStore) AcceptanceTransaction(ctx context.Context, id string) (string, string, error) {
	return s.Store.AcceptanceTransaction(ctx, id)
}

func (s *acceptanceGateStore) SaveProtectedMetadata(ctx context.Context, id string, facts []candidate.ProtectedMetadataFact) error {
	return s.Store.SaveProtectedMetadata(ctx, id, facts)
}

func (s *acceptanceGateStore) LoadProtectedMetadata(ctx context.Context, id string) ([]candidate.ProtectedMetadataFact, error) {
	return s.Store.LoadProtectedMetadata(ctx, id)
}

func (s *acceptanceGateStore) SaveAcceptanceRootIdentities(ctx context.Context, id, expected, target string) error {
	return s.Store.SaveAcceptanceRootIdentities(ctx, id, expected, target)
}

func TestWorkspaceExportWaitsForConcurrentAcceptanceAndRequiresReboundRootIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	for _, path := range []string{formal, baseline, checkout} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "seed.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseline, "seed.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "writer.txt"), []byte("workspace change"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}

	// Prepare an independently reviewed acceptance that will change formal
	// after export has observed and copied its old state.
	acceptanceCandidate, err := candidate.CreateCandidateForPolicy(
		"accept-before-export-completes", formalAbs, filepath.Join(root, ".stable-candidates"), candidate.ManifestPolicyProject,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acceptanceCandidate.CandidateRoot, "accepted.txt"), []byte("accepted change"), 0600); err != nil {
		t.Fatal(err)
	}
	acceptanceCandidate, err = candidate.FreezeCandidate(acceptanceCandidate, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	acceptanceCandidate.Status = "reviewed"
	acceptanceCandidate.ManifestPolicy = candidate.ManifestPolicyProject
	review, err := candidate.BuildReview(ctx, acceptanceCandidate, []candidate.Checker{passingWorkspaceChecker{}})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "0123456789abcdef0123456789abcdef"
	goalID := "session-" + sessionID
	if err := state.SaveCandidate(ctx, store.CandidateRecord{Candidate: acceptanceCandidate, ActionID: "audit-action", GoalID: goalID}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveCandidateReview(ctx, review); err != nil {
		t.Fatal(err)
	}
	decision := candidate.AcceptanceDecision{
		ID: "audit-accept-decision", UserID: "audit-user", CandidateID: acceptanceCandidate.ID,
		CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest,
		FormalDigest: review.FormalDigest, Mode: candidate.AcceptNormal,
	}
	gate := &acceptanceGateStore{Store: state, entered: make(chan struct{}, 1), release: make(chan struct{})}
	acceptResult := make(chan error, 1)
	go func() {
		_, acceptErr := candidate.AcceptCandidate(ctx, acceptanceCandidate, review, decision, goalID, "", gate, time.Now().UTC())
		acceptResult <- acceptErr
	}()
	<-gate.entered // AcceptCandidate now holds the formal-root lock.

	scope := workspace.Scope{
		ProjectID: "project1", SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Authority: permission.Authority{
			RunID: "origin-run", SessionID: sessionID, AllowedRoot: formalAbs,
			FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "ordinary-candidate"),
		},
	}
	formalIdentity, err := workspace.CaptureRootIdentity(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	record := workspace.Record{Scope: scope, Snapshot: workspace.Snapshot{ID: "1123456789abcdef0123456789abcdef", Generation: 1}, FormalRootIdentity: formalIdentity}
	exporter := workspaceCandidateExporter{service: &Service{deps: Deps{Store: state}}}
	paths := workspace.Paths{FormalRoot: formalAbs, Baseline: baseline, Checkout: checkout}
	exportStarted := make(chan struct{})
	exportResult := make(chan struct {
		snapshot workspace.Snapshot
		err      error
	}, 1)
	go func() {
		close(exportStarted)
		snapshot, exportErr := exporter.ExportWorkspace(ctx, scope, record, paths)
		exportResult <- struct {
			snapshot workspace.Snapshot
			err      error
		}{snapshot: snapshot, err: exportErr}
	}()
	<-exportStarted
	select {
	case result := <-exportResult:
		close(gate.release)
		t.Fatalf("export completed while acceptance held the formal-root lock: snapshot=%+v err=%v", result.snapshot, result.err)
	case <-time.After(40 * time.Millisecond):
	}

	close(gate.release)
	if err := <-acceptResult; err != nil {
		t.Fatalf("accept candidate: %v", err)
	}
	exportOutcome := <-exportResult
	if exportOutcome.err == nil {
		t.Fatalf("export accepted a stale formal-root identity after concurrent acceptance: snapshot=%+v", exportOutcome.snapshot)
	}
	if !errors.Is(exportOutcome.err, workspace.ErrOwnership) {
		t.Fatalf("stale identity export error=%v, want ownership failure", exportOutcome.err)
	}
	// In production workspaceService replays the finalized acceptance journal
	// and updates the private receipt before returning the lifecycle manager.
	record.FormalRootIdentity, err = workspace.CaptureRootIdentity(formalAbs)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := exporter.ExportWorkspace(ctx, scope, record, paths)
	if err != nil || exported.CandidateID == "" {
		t.Fatalf("export after authorized root rebind: snapshot=%+v err=%v", exported, err)
	}
	stored, err := state.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Candidate.Status != "ready" {
		t.Fatalf("exported candidate status=%q, want ready", stored.Candidate.Status)
	}
	reviewableCandidate := stored.Candidate
	reviewableCandidate.Status = "frozen"
	exportReview, err := candidate.BuildReview(ctx, reviewableCandidate, []candidate.Checker{passingWorkspaceChecker{}})
	if err != nil {
		t.Fatalf("review exported candidate: %v", err)
	}
	_, currentFormalDigest, err := candidate.BuildManifestForPolicy(formalAbs, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if exportReview.FormalDigest != currentFormalDigest || stored.Candidate.BaselineDigest != currentFormalDigest {
		t.Fatalf("export review formal=%s candidate baseline=%s current formal=%s; export must use post-acceptance state", exportReview.FormalDigest, stored.Candidate.BaselineDigest, currentFormalDigest)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "accepted.txt")); err != nil || string(got) != "accepted change" {
		t.Fatalf("accepted formal change=%q err=%v", got, err)
	}
}
