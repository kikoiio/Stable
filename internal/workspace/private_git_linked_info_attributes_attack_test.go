//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrivateGitLinkedWorktreeIgnoresExternalInfoAttributes proves that a
// linked source worktree's common-dir attributes and filter config are never
// consulted while creating the private baseline and checkout.
func TestPrivateGitLinkedWorktreeIgnoresExternalInfoAttributes(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "fixture-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	sourceRepo := filepath.Join(parent, "source-repository")
	formal := filepath.Join(parent, "formal-worktree")
	if err := os.Mkdir(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "init", "--initial-branch=main")
	fixtureFile(t, sourceRepo, "data.txt", "committed version\n", 0600)
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "add", "data.txt")
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "-c", "user.name=M09 Fixture", "-c", "user.email=m09@example.invalid", "commit", "-m", "fixture baseline")
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "worktree", "add", "--detach", formal, "HEAD")

	const sourceBytes = "current linked worktree bytes\n"
	fixtureFile(t, formal, "data.txt", sourceBytes, 0600)
	commonDir := filepath.Join(sourceRepo, ".git")
	marker := filepath.Join(parent, "external-filter-ran")
	fixtureFile(t, commonDir, "info/attributes", "data.txt filter=hostile\n", 0600)
	clean := "sh -c 'cat >/dev/null; touch " + shellQuoteFixture(marker) + "'"
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "config", "--local", "filter.hostile.clean", clean)
	runIsolatedFixtureGit(t, home, "-C", sourceRepo, "config", "--local", "filter.hostile.required", "true")

	worktreeGitDir := strings.TrimSpace(runIsolatedFixtureGit(t, home, "-C", formal, "rev-parse", "--absolute-git-dir"))
	pointerPath := filepath.Join(formal, ".git")
	protectedPaths := []string{
		pointerPath,
		filepath.Join(commonDir, "config"),
		filepath.Join(commonDir, "info/attributes"),
		filepath.Join(commonDir, "refs/heads/main"),
		filepath.Join(worktreeGitDir, "HEAD"),
		filepath.Join(worktreeGitDir, "index"),
		filepath.Join(worktreeGitDir, "gitdir"),
		filepath.Join(worktreeGitDir, "commondir"),
	}
	type savedFile struct {
		info os.FileInfo
		data []byte
	}
	before := make(map[string]savedFile, len(protectedPaths))
	for _, path := range protectedPaths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("missing linked Git metadata sentinel %q: %v", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read linked Git metadata sentinel %q: %v", path, err)
		}
		before[path] = savedFile{info: info, data: data}
	}

	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = layout.Close() })
	store, err := NewOwnershipStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewPrivateGit(layout, store, Limits{})
	if errors.Is(err, ErrUnavailable) {
		t.Skip("fixed system Git is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	const workspaceID = "linked-info-attributes"
	if _, err := store.Create(context.Background(), scope, workspaceID, "linked attributes fixture", "create-linked-attributes"); err != nil {
		t.Fatal(err)
	}
	state, err := g.Materialize(context.Background(), scope, workspaceID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external common-dir clean filter ran during materialization: %v", err)
	}
	paths, err := layout.Paths(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(paths.Checkout, "data.txt")); err != nil || string(got) != sourceBytes {
		t.Fatalf("private checkout bytes=%q err=%v, want exact source bytes %q", got, err, sourceBytes)
	}
	if got := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":data.txt"); got != sourceBytes {
		t.Fatalf("private baseline bytes=%q, want exact source bytes %q", got, sourceBytes)
	}
	for path, saved := range before {
		afterInfo, statErr := os.Lstat(path)
		afterData, readErr := os.ReadFile(path)
		if statErr != nil || readErr != nil || !os.SameFile(saved.info, afterInfo) || string(saved.data) != string(afterData) {
			t.Fatalf("formal linked Git metadata changed at %q: before-info=%v after-info=%v stat=%v read=%v", path, saved.info, afterInfo, statErr, readErr)
		}
	}
}

// The fixture runs Git with an allowlisted environment, so inherited user
// repository/config/attribute settings cannot redirect setup commands.
func runIsolatedFixtureGit(t *testing.T, home string, args ...string) string {
	t.Helper()
	command := exec.Command("/usr/bin/git", args...)
	command.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated fixture git %v failed: %v\n%s", args, err, output)
	}
	return string(output)
}

func shellQuoteFixture(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
