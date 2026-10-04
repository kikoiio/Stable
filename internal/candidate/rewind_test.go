package candidate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageSwapAndCleanup(t *testing.T) {
	project := t.TempDir()
	store, err := NewSnapshotStore(project, 1<<20, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	candidateRoot := filepath.Join(parent, "cand-1")
	if err := os.Mkdir(candidateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	writeCandidateFile(t, candidateRoot, "a.txt", "one")
	snap, err := store.Create("sess-1", "cand-1", "", "base", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	// The candidate moves on after the snapshot.
	writeCandidateFile(t, candidateRoot, "a.txt", "two")
	writeCandidateFile(t, candidateRoot, "b.txt", "new file")

	staging, err := StageRewind(store, snap, candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(staging) != parent {
		t.Fatalf("staging %q is not a sibling of the candidate", staging)
	}
	if err := SwapWithStaging(candidateRoot, staging); err != nil {
		t.Fatal(err)
	}
	_, digest, err := BuildManifest(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if digest != snap.Digest {
		t.Fatal("candidate did not return to the snapshot")
	}
	content, err := os.ReadFile(filepath.Join(candidateRoot, "a.txt"))
	if err != nil || string(content) != "one" {
		t.Fatalf("restored content = %q, %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(candidateRoot, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("file created after the snapshot survived the rewind")
	}
	// The old candidate content sits in staging for cleanup.
	if _, err := os.Lstat(filepath.Join(staging, "b.txt")); err != nil {
		t.Fatal("previous candidate content lost before cleanup")
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
}

func TestGuardRewindTargetRejectsFormalRoot(t *testing.T) {
	formal := t.TempDir()
	if err := GuardRewindTarget(formal, formal); err == nil {
		t.Fatal("formal root accepted as rewind target")
	}
	if err := GuardRewindTarget(formal, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
