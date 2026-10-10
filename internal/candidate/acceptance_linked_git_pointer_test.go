package candidate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectV2AcceptancePreservesLinkedGitPointerIdentity(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidateParent := filepath.Join(root, "candidates")
	gitDir := filepath.Join(root, "git-common", "worktrees", "formal")
	for _, path := range []string{formal, gitDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	gitPointer := filepath.Join(formal, ".git")
	gitPointerBytes := []byte("gitdir: " + gitDir + "\n")
	if err := os.WriteFile(gitPointer, gitPointerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	originalPointer, err := os.Lstat(gitPointer)
	if err != nil || !originalPointer.Mode().IsRegular() {
		t.Fatalf("linked Git pointer fixture=%v err=%v", originalPointer, err)
	}

	c, err := CreateCandidateForPolicy("linked-git-accept", formal, candidateParent, ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(c.CandidateRoot, ".git")); !os.IsNotExist(err) {
		t.Fatalf("candidate copied the formal Git pointer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.CandidateRoot, "source.txt"), []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = FreezeCandidate(c, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	review, err := BuildReview(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "reviewed"
	decision := AcceptanceDecision{
		ID: "accept-linked-git-pointer", UserID: "test-user", CandidateID: c.ID,
		CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest,
		FormalDigest: review.FormalDigest, Mode: AcceptNormal,
	}
	if _, err := AcceptCandidate(context.Background(), c, review, decision, "session-test", "", &memoryAcceptance{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	finalPointer := filepath.Join(formal, ".git")
	finalInfo, err := os.Lstat(finalPointer)
	if err != nil || !finalInfo.Mode().IsRegular() {
		t.Fatalf("accepted root did not restore a regular .git pointer: info=%v err=%v", finalInfo, err)
	}
	if !os.SameFile(originalPointer, finalInfo) {
		t.Fatal("acceptance replaced the linked .git pointer inode")
	}
	gotPointer, err := os.ReadFile(finalPointer)
	if err != nil || string(gotPointer) != string(gitPointerBytes) {
		t.Fatalf("linked .git pointer changed: bytes=%q err=%v", gotPointer, err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "source.txt")); err != nil || string(got) != "accepted" {
		t.Fatalf("accepted project content=%q err=%v", got, err)
	}
}
