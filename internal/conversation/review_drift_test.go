package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func reviewDriftFixture(t *testing.T) (context.Context, *Service, *store.Store, string, candidate.Candidate) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	formal, candidateRoot := filepath.Join(root, "project"), filepath.Join(root, "candidate")
	for _, dir := range []string{formal, candidateRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := sessionlog.Create(root, "review-drift")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "review-drift-goal", AllowedRoot: formal, SourceSessionID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	_, baseline, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := candidate.BuildManifest(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	record := candidate.Candidate{ID: "review-drift-candidate", FormalRoot: formal, CandidateRoot: candidateRoot, BaselineDigest: baseline, CandidateDigest: digest, Status: "ready"}
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: record, ActionID: "review-drift-action", GoalID: "review-drift-goal"}); err != nil {
		t.Fatal(err)
	}
	return ctx, &Service{deps: Deps{Store: state, ProjectRoot: root}}, state, owner.ID, record
}

func TestReviewRejectsCandidateDigestDrift(t *testing.T) {
	ctx, service, _, sessionID, record := reviewDriftFixture(t)
	if err := os.WriteFile(filepath.Join(record.CandidateRoot, "board"), []byte("changed after ready"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reviewCandidate(ctx, record.ID, sessionID); err == nil {
		t.Fatal("review accepted candidate changed after ready")
	}
}

func TestReviewRejectsFormalBaselineDrift(t *testing.T) {
	ctx, service, _, sessionID, record := reviewDriftFixture(t)
	if err := os.WriteFile(filepath.Join(record.FormalRoot, "board"), []byte("formal changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reviewCandidate(ctx, record.ID, sessionID); err == nil {
		t.Fatal("review accepted formal project changed after candidate preparation")
	}
}
