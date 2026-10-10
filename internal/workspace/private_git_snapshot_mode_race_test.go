//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateGitMaterializeRejectsSourceModeChangeAfterBaselineCapture(t *testing.T) {
	g, layout, store, scope, id := privateGitFixture(t)
	const original = "same source bytes\n"
	const originalMode = os.FileMode(0600)
	if err := os.WriteFile(filepath.Join(layout.FormalRoot(), "mode-sensitive.txt"), []byte(original), originalMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(layout.FormalRoot(), "mode-sensitive.txt"), originalMode); err != nil {
		t.Fatal(err)
	}

	realRun := g.runGit
	mutated := false
	g.runGit = func(ctx context.Context, invocation gitInvocation) ([]byte, error) {
		out, err := realRun(ctx, invocation)
		if err == nil && !mutated && len(invocation.Args) > 0 && invocation.Args[0] == "fast-import" {
			mutated = true
			if chmodErr := os.Chmod(filepath.Join(layout.FormalRoot(), "mode-sensitive.txt"), 0700); chmodErr != nil {
				return nil, chmodErr
			}
		}
		return out, err
	}

	_, err := g.Materialize(context.Background(), scope, id)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("mode-only source change returned %v, want ErrSourceChanged (mutated=%t)", err, mutated)
	}
	if !mutated {
		t.Fatal("mode mutation did not occur after baseline capture")
	}
	info, err := os.Stat(filepath.Join(layout.FormalRoot(), "mode-sensitive.txt"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("source mode fixture did not change: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(layout.FormalRoot(), "mode-sensitive.txt")); err != nil || string(got) != original {
		t.Fatalf("mode mutation changed source bytes: got=%q err=%v", got, err)
	}
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"baseline", "repo.git", "checkout", "run", gitStateName} {
		if _, err := os.Lstat(filepath.Join(paths.Root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mode-only source change published or retained %s: %v", name, err)
		}
	}
	if _, err := store.Load(context.Background(), scope, id); err != nil {
		t.Fatalf("failed materialization lost its ownership intent: %v", err)
	}
}
