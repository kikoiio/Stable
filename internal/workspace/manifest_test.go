//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func fixtureFile(t *testing.T, root, name, data string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotIncludesDataAndSkipsProtectedMetadata(t *testing.T) {
	source := t.TempDir()
	fixtureFile(t, source, "dirty.txt", "current dirty bytes", 0640)
	fixtureFile(t, source, "untracked.txt", "new", 0600)
	fixtureFile(t, source, ".gitignore", "untracked.txt", 0600)
	fixtureFile(t, source, "nested/run.sh", "#!/bin/sh\n", 0755)
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		if err := os.Mkdir(filepath.Join(source, name), 0700); err != nil {
			t.Fatal(err)
		}
		// A FIFO under metadata would fail or block if traversal read it.
		if err := unix.Mkfifo(filepath.Join(source, name, "secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "baseline")
	if err := os.Chmod(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	manifest, err := CopySnapshot(context.Background(), source, target, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.PolicyVersion != ManifestPolicy || len(manifest.Entries) != 4 {
		t.Fatalf("incorrect snapshot: %+v", manifest)
	}
	for _, entry := range manifest.Entries {
		if ProtectedRoot(entry.Path) {
			t.Fatalf("copied metadata: %s", entry.Path)
		}
	}
	data, err := os.ReadFile(filepath.Join(target, "dirty.txt"))
	if err != nil || string(data) != "current dirty bytes" {
		t.Fatalf("lost working bytes: %s, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(target, "nested/run.sh"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("lost executable mode: %v, %v", info, err)
	}
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("metadata copied: %s, %v", name, err)
		}
	}
	if _, err := CopySnapshot(context.Background(), source, target, Limits{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing target overwritten: %v", err)
	}
}

func TestManifestRejectsUnsafeEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "nested-git", "submodule", "metadata-link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			fixtureFile(t, root, "data", "safe", 0600)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("data", filepath.Join(root, "link"))
			case "hardlink":
				err = os.Link(filepath.Join(root, "data"), filepath.Join(root, "link"))
			case "fifo":
				err = unix.Mkfifo(filepath.Join(root, "pipe"), 0600)
			case "nested-git":
				err = os.MkdirAll(filepath.Join(root, "nested", ".Git"), 0700)
			case "submodule":
				fixtureFile(t, root, ".gitmodules", "[submodule \"s\"]\npath = s\n", 0600)
			case "metadata-link":
				err = os.Symlink("data", filepath.Join(root, ".git"))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = BuildManifest(context.Background(), root, Limits{})
			if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrUnsupportedSubmodule) {
				t.Fatalf("unsafe entry accepted: %v", err)
			}
		})
	}
}

func TestManifestBoundedAndCanceled(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "a", "1234", 0600)
	fixtureFile(t, root, "b", "1234", 0600)
	for _, limits := range []Limits{{MaxFiles: 1}, {MaxFileBytes: 3}, {MaxSnapshotBytes: 7}, {MaxEntries: 1}} {
		if _, err := BuildManifest(context.Background(), root, limits); !errors.Is(err, ErrQuota) {
			t.Fatalf("limits not enforced: %+v: %v", limits, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildManifest(ctx, root, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err := CopySnapshot(context.Background(), root, filepath.Join(root, "child"), Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("nested target accepted: %v", err)
	}
}

func TestRootReplacementAndIntermediateSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, root, ".git/secret", "secret", 0600)
	if err := os.Symlink(".git", filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	handle, identity, err := openVerifiedRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if file, err := openBeneath(handle, "data/secret", false); err == nil {
		file.Close()
		t.Fatal("intermediate symlink exposed metadata")
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := revalidateRoot(root, identity); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("replacement accepted: %v", err)
	}
}

func TestManifestDigestRejectsExternalUnsafePaths(t *testing.T) {
	m, err := BuildManifest(context.Background(), t.TempDir(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../x", "a/../x", ".git/x", "dir/.git/x", ".stable/x", "a\\b"} {
		if _, err := ManifestDigest([]ManifestEntry{{Path: path, Digest: m.Digest, Mode: 0600}}); err == nil {
			t.Errorf("invalid manifest path accepted: %q", path)
		}
	}
}

func TestDiskUsageChargesPrivateMetadataAndAllocatedBytes(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "repo.git/objects/private", "object", 0600)
	fixtureFile(t, root, "run/output", "output", 0600)
	used, err := DiskUsage(context.Background(), root, Limits{})
	if err != nil || used <= 12 {
		t.Fatalf("private metadata/allocation omitted: %d, %v", used, err)
	}
	if _, err := DiskUsage(context.Background(), root, Limits{MaxWorkspaceBytes: used - 1}); !errors.Is(err, ErrQuota) {
		t.Fatalf("actual disk usage limit not enforced: %v", err)
	}
	if err := os.Symlink("../run/output", filepath.Join(root, "repo.git", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := DiskUsage(context.Background(), root, Limits{}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("private metadata link accepted: %v", err)
	}
}
