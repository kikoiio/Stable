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

func TestReviewEntryPersistsUnavailableCheckerAndBlocksNormalAccept(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	formal, work := filepath.Join(root, "project"), filepath.Join(root, "candidate")
	for _, dir := range []string{formal, work} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(work, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "g", AllowedRoot: formal}); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "owned-goal", AllowedRoot: formal, SourceSessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	_, base, _ := candidate.BuildManifest(formal)
	_, digest, _ := candidate.BuildManifest(work)
	c := candidate.Candidate{ID: "c", FormalRoot: formal, CandidateRoot: work, BaselineDigest: base, CandidateDigest: digest, Status: "ready"}
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: c, ActionID: "a", GoalID: "owned-goal"}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{Store: state, ProjectRoot: root}}
	review, err := svc.reviewCandidate(ctx, "c", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Findings) != 1 || review.Findings[0].Result != candidate.FindingUnavailable {
		t.Fatalf("missing checker was not surfaced: %+v", review.Findings)
	}
	if review.Digest == "" {
		t.Fatal("review has no digest")
	}
	decision := ClientMsg{CandidateID: "c", SessionID: session.ID, DecisionID: "d", PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest, FormalDigest: review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal)}
	if _, err = svc.acceptReviewedCandidate(ctx, decision); err == nil {
		t.Fatal("normal acceptance bypassed unavailable checker")
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "old" {
		t.Fatalf("formal project changed: %q err=%v", data, err)
	}
}
