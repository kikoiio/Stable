//go:build windows

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsRelativePathClassification(t *testing.T) {
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{name: "nested", path: "src\\main.go", ok: true},
		{name: "slash nested", path: "src/main.go", ok: true},
		{name: "absolute drive", path: `C:\project\main.go`},
		{name: "absolute UNC", path: `\\server\share\main.go`},
		{name: "parent", path: `src\..\main.go`},
		{name: "alternate stream", path: `main.go:secret`},
		{name: "empty", path: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateWindowsRelative(tt.path)
			if tt.ok && err != nil {
				t.Fatalf("validateWindowsRelative(%q): %v", tt.path, err)
			}
			if !tt.ok && !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("validateWindowsRelative(%q) = %v, want ErrUnsafePath", tt.path, err)
			}
		})
	}
}

func TestWindowsExchangeReportsJournaledMoveCapability(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Mkdir(a, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0700); err != nil {
		t.Fatal(err)
	}
	err := exchange(a, b)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("exchange error = %v, want ErrUnsupported", err)
	}
	if got := windowsDirectoryMoveMode(); got != windowsJournaledMove {
		t.Fatalf("windowsDirectoryMoveMode() = %q, want %q", got, windowsJournaledMove)
	}
}

func TestWindowsRootWriteAndRemove(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.MkdirAll(`stable\memory`, 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic(`stable\memory\item.md`, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileAtomic(`stable\memory\item.md`, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "stable", "memory", "item.md"))
	if err != nil || string(got) != "two" {
		t.Fatalf("atomic replacement = %q, %v", got, err)
	}
	if err := root.RemoveFile(`stable\memory\item.md`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "stable", "memory", "item.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file remains after remove: %v", err)
	}
	for _, rel := range []string{`..\outside`, `C:\outside`} {
		if err := root.WriteFileAtomic(rel, []byte("no"), 0600); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("WriteFileAtomic(%q) = %v, want ErrUnsafePath", rel, err)
		}
	}
}
