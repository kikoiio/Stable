//go:build linux

package secfile

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinuxSecureOpenRejectsTraversalAndSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "file.txt"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := SecureOpen(root, "nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for _, rel := range []string{"../file.txt", "/tmp/file.txt", "nested/../file.txt", ""} {
		if _, err := SecureOpen(root, rel); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("SecureOpen(%q) = %v, want ErrUnsafePath", rel, err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "nested"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := SecureOpen(root, "link/file.txt"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink traversal error = %v, want ErrUnsafePath", err)
	}
}

func TestLinuxMoveDirectoryChecksAndMoves(t *testing.T) {
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

func TestLinuxExchangeRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	a := filepath.Join(parent, "a")
	b := filepath.Join(parent, "b")
	if err := os.Mkdir(a, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if err := ExchangeDirectories(a, b); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ExchangeDirectories error = %v, want ErrUnsafePath", err)
	}
}

func TestLinuxSecureOpenRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "role.md"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := SecureOpen(root, "role.md")
		if file != nil {
			file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("FIFO open error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("special-file open blocked waiting for a writer")
	}
}
