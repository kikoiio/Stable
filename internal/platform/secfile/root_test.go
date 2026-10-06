package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRootAndRevalidate(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path() != filepath.Clean(rootPath) {
		t.Fatalf("root path = %q, want %q", root.Path(), filepath.Clean(rootPath))
	}
	if err := root.Revalidate(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRootRejectsSymlinkAndFile(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRoot(file); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("file root error = %v, want ErrUnsafePath", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(filepath.Join(parent, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRoot(link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink root error = %v, want ErrUnsafePath", err)
	}
}

func TestRootRevalidateDetectsReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "project")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(parent, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.Revalidate(); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("replacement error = %v, want ErrRootChanged", err)
	}
}
