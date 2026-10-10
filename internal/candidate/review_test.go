package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiffManifestsRejectsUnsafeEntryPath(t *testing.T) {
	_, err := DiffManifests(t.TempDir(), t.TempDir(), []ManifestEntry{{
		Path:   "../outside.txt",
		Mode:   0600,
		Size:   1,
		Digest: "not-a-real-digest",
	}}, nil)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("DiffManifests error = %v, want ErrUnsafePath", err)
	}
}

func TestLegacyReviewJSONKeepsHistoricalDigestAndAcceptance(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidateRoot := filepath.Join(root, "candidate")
	for _, dir := range []string{formal, candidateRoot} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "note.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "note.txt"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	_, formalDigest, err := BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, candidateDigest, err := BuildManifest(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	review := Review{
		ID:              "review-legacy",
		CandidateID:     "legacy",
		FormalDigest:    formalDigest,
		CandidateDigest: candidateDigest,
		Changes:         []FileChange{{Path: "note.txt", Status: "modified"}},
		Findings:        []Finding{{ID: "review-check", Checker: "checker", Version: "1", Result: FindingFail}},
	}
	// Historical review digests did not include a manifest policy field.
	legacyBytes, err := json.Marshal(struct {
		Candidate string
		Formal    string
		Changes   []FileChange
		Findings  []Finding
	}{review.CandidateDigest, review.FormalDigest, review.Changes, review.Findings})
	if err != nil {
		t.Fatal(err)
	}
	historicalSum := sha256.Sum256(legacyBytes)
	review.Digest = hex.EncodeToString(historicalSum[:])
	storedJSON, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	var storedFields map[string]json.RawMessage
	if err := json.Unmarshal(storedJSON, &storedFields); err != nil {
		t.Fatal(err)
	}
	if _, exists := storedFields["manifest_policy"]; exists {
		t.Fatal("legacy review JSON unexpectedly contains manifest_policy")
	}

	var loaded Review
	if err := json.Unmarshal(storedJSON, &loaded); err != nil {
		t.Fatal(err)
	}
	recomputed, err := ComputeReviewDigest(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != review.Digest {
		t.Fatalf("recomputed legacy digest = %s, want historical digest %s", recomputed, review.Digest)
	}
	if loaded.ManifestPolicy != "" {
		t.Fatalf("loading legacy review filled manifest policy %q", loaded.ManifestPolicy)
	}

	c := Candidate{ID: "legacy", FormalRoot: formal, CandidateRoot: candidateRoot, Status: "reviewed"}
	d := AcceptanceDecision{
		ID: "decision-legacy", UserID: "user", CandidateID: c.ID,
		CandidateDigest: loaded.CandidateDigest, PreviewDigest: loaded.Digest,
		FormalDigest: loaded.FormalDigest, Mode: AcceptForce,
		ConfirmedFindings: []string{"review-check"},
	}
	if err := validateAcceptance(context.Background(), c, loaded, d); err != nil {
		t.Fatalf("acceptance rejected historical review with explicit old finding confirmation: %v", err)
	}
}
