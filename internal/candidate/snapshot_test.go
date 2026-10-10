package candidate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCandidateFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func newCandidateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeCandidateFile(t, root, "src/main.go", "package main\n")
	writeCandidateFile(t, root, "README.md", "hello\n")
	return root
}

func newStore(t *testing.T, maxBytes int64, maxManifests int) (*SnapshotStore, string) {
	t.Helper()
	project := t.TempDir()
	store, err := NewSnapshotStore(project, maxBytes, maxManifests, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, project
}

func TestSnapshotCreateListAndOwnership(t *testing.T) {
	store, project := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	snap, err := store.Create("sess-1", "cand-1", "run-1", "before write", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Digest == "" || len(snap.Entries) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	listed, err := store.List("cand-1")
	if err != nil || len(listed) != 1 || listed[0].SnapshotID != snap.SnapshotID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	// Other candidates and other sessions cannot see this snapshot.
	if other, err := store.List("cand-2"); err != nil || len(other) != 0 {
		t.Fatalf("cross-candidate list = %+v, %v", other, err)
	}
	if _, err = store.ValidateRestore("cand-2", snap.SnapshotID); err == nil {
		t.Fatal("cross-candidate restore validated")
	}
	// A second store instance (restart) sees the same snapshot.
	reopened, err := NewSnapshotStore(project, 1<<20, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed, err = reopened.List("cand-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list after restart = %+v, %v", listed, err)
	}
}

func TestSnapshotBlobsAreReusedAcrossSnapshots(t *testing.T) {
	store, _ := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	if _, err := store.Create("sess-1", "cand-1", "", "first", candidateRoot); err != nil {
		t.Fatal(err)
	}
	writeCandidateFile(t, candidateRoot, "src/new.go", "package src\n")
	if _, err := store.Create("sess-1", "cand-1", "", "second", candidateRoot); err != nil {
		t.Fatal(err)
	}
	blobs, err := os.ReadDir(filepath.Join(store.root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 3 {
		t.Fatalf("blobs = %d, want 3 (shared content stored once)", len(blobs))
	}
}

func TestSnapshotQuotasRefuseBeforeWriting(t *testing.T) {
	candidateRoot := newCandidateRoot(t)
	tight, _ := newStore(t, 1, 10)
	if _, err := tight.Create("sess-1", "cand-1", "", "too big", candidateRoot); err == nil {
		t.Fatal("byte quota not enforced")
	}
	if blobs, _ := os.ReadDir(filepath.Join(tight.root, "blobs")); len(blobs) != 0 {
		t.Fatal("blobs written despite quota failure")
	}
	limited, _ := newStore(t, 1<<20, 1)
	if _, err := limited.Create("sess-1", "cand-1", "", "one", candidateRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Create("sess-1", "cand-1", "", "two", candidateRoot); err == nil {
		t.Fatal("manifest quota not enforced")
	}
	// The limit is per candidate; another candidate still snapshots.
	if _, err := limited.Create("sess-1", "cand-2", "", "other", candidateRoot); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRestoreRejectsCorruptOrMissingBlob(t *testing.T) {
	store, _ := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	snap, err := store.Create("sess-1", "cand-1", "", "base", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ValidateRestore("cand-1", snap.SnapshotID); err != nil {
		t.Fatal(err)
	}
	target := snap.Entries[0].Digest
	blob := store.blobPath(target)
	raw, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err = os.WriteFile(blob, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ValidateRestore("cand-1", snap.SnapshotID); err == nil {
		t.Fatal("corrupt blob accepted")
	}
	if err = os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ValidateRestore("cand-1", snap.SnapshotID); err == nil {
		t.Fatal("missing blob accepted")
	}
}

func TestSnapshotRejectsUnsafeIdentifiersAndDigestPaths(t *testing.T) {
	project := t.TempDir()
	store, err := NewSnapshotStore(project, 1<<20, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(".."); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("List traversal error = %v, want ErrUnsafePath", err)
	}
	if _, err := store.ValidateRestore("cand-1", "../escape"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ValidateRestore traversal error = %v, want ErrUnsafePath", err)
	}
	snap := FileSnapshot{
		SnapshotID:  "snap-unsafe",
		ProjectID:   store.projectID,
		CandidateID: "cand-1",
		Digest:      strings.Repeat("a", 64),
		Entries: []ManifestEntry{{
			Path:   "ok.txt",
			Digest: "../escape",
		}},
	}
	if err := store.writeManifestLocked(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateRestore("cand-1", snap.SnapshotID); err == nil {
		t.Fatal("ValidateRestore accepted a digest path escape")
	}
}

func TestMaterializeRestoresExactManifest(t *testing.T) {
	store, _ := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	snap, err := store.Create("sess-1", "cand-1", "", "base", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if err = os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err = store.Materialize(snap, staging); err != nil {
		t.Fatal(err)
	}
	_, digest, err := BuildManifest(staging)
	if err != nil {
		t.Fatal(err)
	}
	if digest != snap.Digest {
		t.Fatal("materialized digest mismatch")
	}
	content, err := os.ReadFile(filepath.Join(staging, "src", "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("restored content = %q, %v", content, err)
	}
}

func TestSnapshotRetainsProjectManifestPolicyAcrossRestore(t *testing.T) {
	store, _ := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	for _, rel := range []string{".git/config", ".stable/session.jsonl", ".mewcode/agents/private.md"} {
		writeCandidateFile(t, candidateRoot, rel, "protected")
	}
	snap, err := store.CreateForPolicy("sess-1", "cand-v2", "run-1", "baseline", candidateRoot, ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ManifestPolicy != ManifestPolicyProject {
		t.Fatalf("snapshot policy = %q", snap.ManifestPolicy)
	}
	loaded, err := store.ValidateRestore("cand-v2", snap.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ManifestPolicy != ManifestPolicyProject {
		t.Fatalf("persisted snapshot policy = %q", loaded.ManifestPolicy)
	}
	staging := t.TempDir()
	if err = store.Materialize(loaded, staging); err != nil {
		t.Fatal(err)
	}
	_, digest, err := BuildManifestForPolicy(staging, ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if digest != snap.Digest {
		t.Fatal("project-policy snapshot restore changed its digest")
	}
	for _, rel := range []string{".git", ".stable", ".mewcode"} {
		if _, err = os.Lstat(filepath.Join(staging, rel)); !os.IsNotExist(err) {
			t.Fatalf("protected metadata %s was materialized: %v", rel, err)
		}
	}
}

func TestMaterializeRejectsTraversalEntry(t *testing.T) {
	store, _ := newStore(t, 1<<20, 10)
	candidateRoot := newCandidateRoot(t)
	snap, err := store.Create("sess-1", "cand-1", "", "base", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	snap.Entries = append(snap.Entries, ManifestEntry{Path: "../escape", Mode: 0600, Size: 0, Digest: snap.Entries[0].Digest})
	staging := t.TempDir()
	if err = store.Materialize(snap, staging); err == nil {
		t.Fatal("traversal entry materialized")
	}
}

func TestSnapshotLabelIsRedacted(t *testing.T) {
	project := t.TempDir()
	store, err := NewSnapshotStore(project, 1<<20, 10, []string{"sk-live-secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	candidateRoot := newCandidateRoot(t)
	snap, err := store.Create("sess-1", "cand-1", "", "key is sk-live-secret-token ok", candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap.Label, "sk-live-secret-token") {
		t.Fatalf("credential reached snapshot metadata: %q", snap.Label)
	}
	raw, err := os.ReadFile(filepath.Join(store.root, "manifests", "cand-1", snap.SnapshotID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live-secret-token") {
		t.Fatal("credential persisted in manifest")
	}
	short, err := NewSnapshotStore(project, 1<<20, 10, []string{"abc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = short.Create("sess-1", "cand-9", "", "contains abc", candidateRoot); err == nil {
		t.Fatal("short credential saved as plaintext")
	}
}
