//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopySnapshotRejectsSameSizeSameModeMutationAfterManifest(t *testing.T) {
	source := t.TempDir()
	const original = "before snapshot\n"
	const replacement = "after snapshot!\n"
	if len(original) != len(replacement) {
		t.Fatal("test strings must have equal size")
	}
	fixtureFile(t, source, "source.txt", original, 0600)
	sourcePath := filepath.Join(source, "source.txt")
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	targetParent := t.TempDir()
	if err := os.Chmod(targetParent, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetParent, "baseline")
	mutated := false
	_, err = copySnapshotWithSync(context.Background(), source, target, Limits{}, func(os.FileInfo) error {
		// The allocation callback runs after BuildManifest and target creation,
		// but before CopySnapshot opens the source file.
		mutated = true
		if err := os.WriteFile(sourcePath, []byte(replacement), 0600); err != nil {
			return err
		}
		return os.Chtimes(sourcePath, before.ModTime(), before.ModTime())
	}, nil)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("same-size snapshot mutation returned %v, want ErrSourceChanged (mutated=%t)", err, mutated)
	}
	if !mutated {
		t.Fatal("post-manifest mutation hook did not run")
	}
	after, err := os.Stat(sourcePath)
	if err != nil || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mutation fixture did not preserve size/mode/mtime: before=%v after=%v err=%v", before, after, err)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != replacement {
		t.Fatalf("source replacement was lost: got %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "source.txt")); err != nil || string(got) != replacement {
		t.Fatalf("fixture did not reach the stale-manifest copy window: got %q err=%v", got, err)
	}
}
