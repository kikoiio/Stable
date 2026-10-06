//go:build darwin

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinSecureOpenRejectsTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "nested", "file.txt")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := SecureOpen(root, "nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	for _, rel := range []string{"../file.txt", "/tmp/file.txt", "nested/../file.txt"} {
		if _, err := SecureOpen(root, rel); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("SecureOpen(%q) error = %v, want ErrUnsafePath", rel, err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "nested"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SecureOpen(root, "link/file.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink traversal error = %v, want ErrUnsafePath", err)
	}
}

func TestDarwinExchangeReportsUnsupportedCapability(t *testing.T) {
	parent := t.TempDir()
	a := filepath.Join(parent, "a")
	b := filepath.Join(parent, "b")
	if err := os.Mkdir(a, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exchange(a, b); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("exchange error = %v, want ErrUnsupported", err)
	}
}

func TestDarwinMoveDirectoryChecksVolumeAndMoves(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := moveDirectory(src, dst, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("moved destination missing: %v", err)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after move error = %v, want not exist", err)
	}
}
