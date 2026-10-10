//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The repository-local info/attributes file has higher precedence than a
// worktree .gitattributes file in ordinary Git commands. Workspace creation
// must treat it as excluded source metadata and build from file bytes alone.
func TestPrivateGitMaterializeIgnoresFormalInfoAttributesFilter(t *testing.T) {
	g, layout, _, scope, id := privateGitFixture(t)
	formal := layout.FormalRoot()
	const source = "raw source bytes\n"
	fixtureFile(t, formal, "data.txt", source, 0600)
	marker := filepath.Join(t.TempDir(), "formal-filter-ran")
	fixtureFile(t, formal, ".git/info/attributes", "data.txt filter=hostile\n", 0600)
	fixtureFile(t, formal, ".git/config", "[filter \"hostile\"]\n\tclean = sh -c 'cat >/dev/null; touch "+marker+"'\n\trequired = true\n", 0600)

	state, err := g.Materialize(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":data.txt"); got != source {
		t.Fatalf("private baseline changed bytes through formal info/attributes: got %q, want %q", got, source)
	}
	if got, err := os.ReadFile(filepath.Join(paths.Checkout, "data.txt")); err != nil || string(got) != source {
		t.Fatalf("checkout changed bytes through formal info/attributes: got %q, err %v", got, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("formal Git filter executed during private materialization: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Repository, "info", "attributes")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("formal info/attributes copied into private repository: %v", err)
	}
}
