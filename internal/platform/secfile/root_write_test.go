//go:build linux

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootMkdirAllWriteFileAtomicAndRemove(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.MkdirAll("stable/memory", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic("stable/memory/a.md", []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic("stable/memory/a.md", []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := root.ReadDir("stable/memory")
	if err != nil || len(entries) != 1 || entries[0].Name() != "a.md" {
		t.Fatalf("ReadDir entries = %v, %v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "stable", "memory", "a.md"))
	if err != nil || string(got) != "second" {
		t.Fatalf("read replaced file = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "stable", "memory", "a.md"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode = %v, %v", info, err)
	}
	if err := root.RemoveFile("stable/memory/a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "stable", "memory", "a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file remains after remove: %v", err)
	}
}

func TestRootWriteAndRemoveRejectUnsafeTargets(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.MkdirAll("safe", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside", "escape/file"} {
		if err := root.WriteFileAtomic(rel, []byte("no"), 0600); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("WriteFileAtomic(%q) error = %v, want ErrUnsafePath", rel, err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic("link", []byte("no"), 0600); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteFileAtomic symlink error = %v, want ErrUnsafePath", err)
	}
	if err := root.RemoveFile("link"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("RemoveFile symlink error = %v, want ErrUnsafePath", err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic("directory", []byte("no"), 0600); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteFileAtomic directory error = %v, want ErrUnsafePath", err)
	}
}
