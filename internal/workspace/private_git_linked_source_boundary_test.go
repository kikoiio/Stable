//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrivateGitMaterializePreservesLinkedFormalWorktree proves that a source
// project's .git pointer is treated as protected project metadata: materialize
// snapshots the current files into its own repository without opening or
// changing the linked worktree's external common directory.
func TestPrivateGitMaterializePreservesLinkedFormalWorktree(t *testing.T) {
	parent := t.TempDir()
	sourceRepo := filepath.Join(parent, "source-repository")
	formal := filepath.Join(parent, "formal-worktree")
	if err := os.Mkdir(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}
	sourceGit(t, sourceRepo, "init", "--initial-branch=main")
	sourceGit(t, sourceRepo, "config", "user.name", "M09 Fixture")
	sourceGit(t, sourceRepo, "config", "user.email", "m09@example.invalid")
	fixtureFile(t, sourceRepo, "tracked.txt", "committed source version\n", 0600)
	fixtureFile(t, sourceRepo, "history-only.txt", "must not become history\n", 0600)
	sourceGit(t, sourceRepo, "add", "tracked.txt", "history-only.txt")
	sourceGit(t, sourceRepo, "commit", "-m", "source history sentinel")
	sourceGit(t, sourceRepo, "branch", "formal-worktree")
	sourceGit(t, sourceRepo, "worktree", "add", "--detach", formal, "refs/heads/formal-worktree")
	fixtureFile(t, formal, "tracked.txt", "current dirty source version\n", 0640)
	fixtureFile(t, formal, "untracked.txt", "current untracked source version\n", 0600)
	if err := os.Remove(filepath.Join(formal, "history-only.txt")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(parent, "external-hook-ran")
	hook := "#!/bin/sh\nprintf touched > '" + marker + "'\n"
	commonDir := filepath.Join(sourceRepo, ".git")
	fixtureFile(t, commonDir, "hooks/post-commit", hook, 0700)

	pointerPath := filepath.Join(formal, ".git")
	pointerInfo, err := os.Lstat(pointerPath)
	if err != nil || !pointerInfo.Mode().IsRegular() {
		t.Fatalf("fixture .git is not a regular pointer: info=%v err=%v", pointerInfo, err)
	}
	worktreeGitDir := strings.TrimSpace(sourceGit(t, formal, "rev-parse", "--absolute-git-dir"))
	gotCommonDir := strings.TrimSpace(sourceGit(t, formal, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(gotCommonDir) {
		gotCommonDir = filepath.Join(formal, gotCommonDir)
	}
	if filepath.Clean(gotCommonDir) != filepath.Clean(commonDir) {
		t.Fatalf("formal worktree common dir=%q, want %q", gotCommonDir, commonDir)
	}

	// Include shared refs/config/hooks and linked-worktree-local HEAD/index in
	// the preservation snapshot. Both file identity and bytes must survive.
	protectedPaths := []string{
		pointerPath,
		filepath.Join(commonDir, "config"),
		filepath.Join(commonDir, "refs", "heads", "main"),
		filepath.Join(commonDir, "refs", "heads", "formal-worktree"),
		filepath.Join(commonDir, "hooks", "post-commit"),
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
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("missing source Git sentinel %q: %v", path, statErr)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read source Git sentinel %q: %v", path, readErr)
		}
		before[path] = savedFile{info: info, data: data}
	}
	refsBefore := sourceGit(t, formal, "for-each-ref", "--format=%(refname):%(objectname)")
	configBefore := sourceGit(t, formal, "config", "--local", "--list", "--null")

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
	const workspaceID = "linked-source"
	if _, err := store.Create(context.Background(), scope, workspaceID, "linked source fixture", "create-linked-source"); err != nil {
		t.Fatal(err)
	}
	state, err := g.Materialize(context.Background(), scope, workspaceID)
	if err != nil {
		t.Fatal(err)
	}

	for path, saved := range before {
		afterInfo, statErr := os.Lstat(path)
		afterData, readErr := os.ReadFile(path)
		if statErr != nil || readErr != nil || !os.SameFile(saved.info, afterInfo) || string(saved.data) != string(afterData) {
			t.Fatalf("formal linked Git metadata changed at %q: before-info=%v after-info=%v stat=%v read=%v", path, saved.info, afterInfo, statErr, readErr)
		}
	}
	if got := sourceGit(t, formal, "for-each-ref", "--format=%(refname):%(objectname)"); got != refsBefore {
		t.Fatalf("formal refs changed: before=%q after=%q", refsBefore, got)
	}
	if got := sourceGit(t, formal, "config", "--local", "--list", "--null"); got != configBefore {
		t.Fatal("formal linked worktree Git configuration changed")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external common-dir hook ran during materialization: %v", err)
	}

	paths, err := layout.Paths(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := BuildManifest(context.Background(), formal, Limits{})
	if err != nil || formalManifest.Digest != state.BaselineDigest {
		t.Fatalf("private baseline digest does not match current linked worktree files: manifest=%+v state=%+v err=%v", formalManifest, state, err)
	}
	for name, want := range map[string]string{
		"tracked.txt":   "current dirty source version\n",
		"untracked.txt": "current untracked source version\n",
	} {
		got := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":"+name)
		if got != want {
			t.Fatalf("private baseline %q=%q, want %q", name, got, want)
		}
	}
	if _, err := gitQueryResult(t, g, paths, paths.Repository, "", "cat-file", "-e", state.BaselineCommit+":history-only.txt"); err == nil {
		t.Fatal("private baseline retained a source-HEAD file absent from the current linked worktree")
	}
	if got := strings.TrimSpace(gitQuery(t, g, paths, paths.Repository, "", "rev-list", "--count", "--all")); got != "1" {
		t.Fatalf("private repository imported source history: commit count=%q", got)
	}
	refs := strings.Fields(gitQuery(t, g, paths, paths.Repository, "", "for-each-ref", "--format=%(refname)"))
	if len(refs) != 2 || refs[0] != baselineRef && refs[1] != baselineRef || refs[0] != checkoutRef && refs[1] != checkoutRef {
		t.Fatalf("private repository refs include source/external refs: %q", refs)
	}
	for _, forbidden := range []string{"objects/info/alternates", "objects/info/http-alternates", "info/grafts", "info/attributes", "refs/replace", "refs/remotes"} {
		if _, err := os.Lstat(filepath.Join(paths.Repository, forbidden)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private repository contains external Git metadata %q: %v", forbidden, err)
		}
	}
	privateConfig, err := os.ReadFile(filepath.Join(paths.Repository, "config"))
	if err != nil || string(privateConfig) != privateGitConfiguration || strings.Contains(string(privateConfig), sourceRepo) {
		t.Fatalf("private repository config references source common dir: %q err=%v", privateConfig, err)
	}
	if got := strings.TrimSpace(gitQuery(t, g, paths, paths.Repository, "", "remote")); got != "" {
		t.Fatalf("private repository has source/external remotes: %q", got)
	}
	for _, object := range gitObjectFiles(t, filepath.Join(paths.Repository, "objects")) {
		for _, sourceObject := range gitObjectFiles(t, filepath.Join(commonDir, "objects")) {
			if os.SameFile(object, sourceObject) {
				t.Fatal("private repository hardlinked an object from the formal common directory")
			}
		}
	}
}

// gitQueryResult is the error-preserving companion used when absence itself is
// the assertion; gitQuery intentionally fails the test on any command error.
func gitQueryResult(t *testing.T, g *PrivateGit, paths Paths, gitDir, worktree string, args ...string) (string, error) {
	t.Helper()
	out, err := g.runGit(context.Background(), gitInvocation{Paths: paths, GitDir: gitDir, WorkTree: worktree, Args: args})
	return string(out), err
}
