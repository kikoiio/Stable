//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPrivateGitMaterializeRejectsFormalRootReplacement verifies the full
// materialization transaction binds its receipt to the original formal root,
// even when a replacement has identical file contents and therefore the same
// manifest digest.
func TestPrivateGitMaterializeRejectsFormalRootReplacement(t *testing.T) {
	g, layout, _, scope, id := privateGitFixture(t)
	formal := layout.FormalRoot()
	fixtureFile(t, formal, "data.txt", "stable source bytes\n", 0600)
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	oldFormal := formal + "-original"
	realRun := g.runGit
	replaced := false
	g.runGit = func(ctx context.Context, invocation gitInvocation) ([]byte, error) {
		out, runErr := realRun(ctx, invocation)
		if runErr == nil && !replaced && len(invocation.Args) > 0 && invocation.Args[0] == "fast-import" {
			if err := os.Rename(formal, oldFormal); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(formal, 0700); err != nil {
				t.Fatal(err)
			}
			fixtureFile(t, formal, "data.txt", "stable source bytes\n", 0600)
			replaced = true
		}
		return out, runErr
	}

	_, err = g.Materialize(context.Background(), scope, id)
	if !replaced {
		t.Fatal("fixture did not replace the formal root during materialization")
	}
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("formal root replacement was not rejected as source change: %v", err)
	}
	for _, name := range []string{"baseline", "repo.git", "checkout", "run", gitStateName} {
		if _, statErr := os.Lstat(filepath.Join(paths.Root, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed materialization published or retained owned resource %q: %v", name, statErr)
		}
	}
	if got, readErr := os.ReadFile(filepath.Join(oldFormal, "data.txt")); readErr != nil || string(got) != "stable source bytes\n" {
		t.Fatalf("original formal root was damaged: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(formal, "data.txt")); readErr != nil || string(got) != "stable source bytes\n" {
		t.Fatalf("replacement formal root was damaged: %q, %v", got, readErr)
	}
}
