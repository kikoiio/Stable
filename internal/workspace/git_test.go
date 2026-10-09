//go:build linux || darwin

package workspace

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func privateGitFixture(t *testing.T) (*PrivateGit, *Layout, *OwnershipStore, Scope, string) {
	t.Helper()
	layout, store := ownershipFixture(t)
	g, err := NewPrivateGit(layout, store, Limits{})
	if errors.Is(err, ErrUnavailable) {
		t.Skip("fixed system Git is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	id := "workspace"
	if _, err := store.Create(context.Background(), scope, id, "label / is not a Git ref", "create"); err != nil {
		t.Fatal(err)
	}
	return g, layout, store, scope, id
}

func gitQuery(t *testing.T, g *PrivateGit, paths Paths, gitDir, worktree string, args ...string) string {
	t.Helper()
	out, err := g.runGit(context.Background(), gitInvocation{Paths: paths, GitDir: gitDir, WorkTree: worktree, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func sourceGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	command := exec.Command("git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return string(output)
}

func gitObjectFiles(t *testing.T, root string) []os.FileInfo {
	t.Helper()
	var files []os.FileInfo
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, info)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPrivateGitMaterializePreservesActualFormalRepository(t *testing.T) {
	g, layout, _, scope, id := privateGitFixture(t)
	formal := layout.FormalRoot()
	sourceGit(t, formal, "init", "--initial-branch=main")
	sourceGit(t, formal, "config", "user.name", "M09 Fixture")
	sourceGit(t, formal, "config", "user.email", "m09@example.invalid")
	sourceGit(t, formal, "config", "remote.origin.url", "https://example.invalid/private")
	fixtureFile(t, formal, ".gitignore", "cache.ignored\n", 0600)
	fixtureFile(t, formal, "tracked.txt", "committed bytes\n", 0600)
	sourceGit(t, formal, "add", ".gitignore", "tracked.txt")
	sourceGit(t, formal, "commit", "-m", "fixture baseline")
	sourceGit(t, formal, "branch", "sentinel")
	fixtureFile(t, formal, ".git/hooks/post-commit", "#!/bin/sh\nprintf sentinel\n", 0700)
	fixtureFile(t, formal, "tracked.txt", "staged bytes\n", 0600)
	sourceGit(t, formal, "add", "tracked.txt")
	fixtureFile(t, formal, "tracked.txt", "unstaged bytes\n", 0600)
	fixtureFile(t, formal, "staged-only.txt", "staged addition\n", 0600)
	sourceGit(t, formal, "add", "staged-only.txt")
	fixtureFile(t, formal, "cache.ignored", "ignored working data\n", 0600)
	if status := sourceGit(t, formal, "status", "--ignored", "--porcelain=v1"); !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "A  staged-only.txt") || !strings.Contains(status, "!! cache.ignored") {
		t.Fatalf("fixture does not contain staged, unstaged, and ignored changes: %q", status)
	}

	formalGit := filepath.Join(formal, ".git")
	preservedPaths := []string{"", "config", "index", "HEAD", filepath.Join("refs", "heads", "main"), filepath.Join("refs", "heads", "sentinel"), filepath.Join("hooks", "post-commit")}
	beforeIdentity := make(map[string]os.FileInfo, len(preservedPaths))
	beforeBytes := make(map[string][]byte, len(preservedPaths)-1)
	for _, relative := range preservedPaths {
		path := formalGit
		if relative != "" {
			path = filepath.Join(formalGit, relative)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("missing formal Git sentinel %q: %v", relative, err)
		}
		beforeIdentity[relative] = info
		if relative != "" {
			beforeBytes[relative], err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	formalRefs := sourceGit(t, formal, "for-each-ref", "--format=%(refname):%(objectname)")
	formalConfig := sourceGit(t, formal, "config", "--local", "--list", "--null")

	state, err := g.Materialize(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range preservedPaths {
		path := formalGit
		if relative != "" {
			path = filepath.Join(formalGit, relative)
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(beforeIdentity[relative], after) {
			t.Fatalf("formal .git identity changed at %q: before=%v after=%v err=%v", relative, beforeIdentity[relative], after, err)
		}
		if relative != "" {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, beforeBytes[relative]) {
				t.Fatalf("formal .git content changed at %q: %v", relative, err)
			}
		}
	}
	if got := sourceGit(t, formal, "for-each-ref", "--format=%(refname):%(objectname)"); got != formalRefs {
		t.Fatalf("formal refs changed: before=%q after=%q", formalRefs, got)
	}
	if got := sourceGit(t, formal, "config", "--local", "--list", "--null"); got != formalConfig {
		t.Fatal("formal local Git configuration changed")
	}
	privateConfig, err := os.ReadFile(filepath.Join(paths.Repository, "config"))
	if err != nil || string(privateConfig) != privateGitConfiguration {
		t.Fatalf("private bare repository inherited source configuration: %q, %v", privateConfig, err)
	}
	if got := gitQuery(t, g, paths, paths.Repository, "", "remote"); strings.TrimSpace(got) != "" {
		t.Fatalf("private bare repository has remotes: %q", got)
	}
	for _, relative := range []string{"objects/info/alternates", "objects/info/http-alternates", "info/grafts", "refs/replace", "refs/remotes"} {
		if _, err := os.Lstat(filepath.Join(paths.Repository, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private bare repository inherited forbidden metadata %q: %v", relative, err)
		}
	}
	sourceObjects := gitObjectFiles(t, filepath.Join(formalGit, "objects"))
	privateObjects := gitObjectFiles(t, filepath.Join(paths.Repository, "objects"))
	for _, privateObject := range privateObjects {
		for _, sourceObject := range sourceObjects {
			if os.SameFile(privateObject, sourceObject) {
				t.Fatal("private bare repository hardlinked a formal object")
			}
		}
	}
	for name, want := range map[string]string{
		"tracked.txt":     "unstaged bytes\n",
		"staged-only.txt": "staged addition\n",
		"cache.ignored":   "ignored working data\n",
	} {
		got := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":"+name)
		if got != want {
			t.Fatalf("synthetic baseline lost working tree bytes for %s: got %q want %q", name, got, want)
		}
	}
}

func TestPrivateGitSyntheticBaselineAndLinkedCheckout(t *testing.T) {
	g, layout, store, scope, id := privateGitFixture(t)
	formal := layout.FormalRoot()
	fixtureFile(t, formal, "dirty.txt", "dirty source bytes\n", 0640)
	fixtureFile(t, formal, "untracked.txt", "untracked source bytes\n", 0600)
	fixtureFile(t, formal, "ignored.txt", "ignored but preserved", 0600)
	fixtureFile(t, formal, ".gitignore", "ignored.txt\n", 0600)
	fixtureFile(t, formal, "scripts/run.sh", "#!/bin/sh\nexit 0\n", 0755)
	fixtureFile(t, formal, "names/换行\n\"file", "quoted path", 0600)
	fixtureFile(t, formal, ".gitmodules", "# no submodules\n", 0600)
	// An attribute filter is deliberately executable-looking project data.
	// Plumbing imports raw bytes and never runs this configuration.
	fixtureFile(t, formal, ".gitattributes", "*.txt filter=hostile text eol=crlf\n", 0600)
	marker := filepath.Join(t.TempDir(), "hook-was-run")
	hook := "#!/bin/sh\nprintf touched > '" + marker + "'\n"
	fixtureFile(t, formal, ".git/hooks/post-commit", hook, 0700)
	config := "[include]\n path = /does-not-exist\n[filter \"hostile\"]\n clean = " + hook + "\n required = true\n[remote \"origin\"]\n url = https://invalid.example/private\n"
	fixtureFile(t, formal, ".git/config", config, 0600)
	fixtureFile(t, formal, ".git/HEAD", "ref: refs/heads/formal\n", 0600)
	fixtureFile(t, formal, ".git/index", "source index bytes", 0600)
	fixtureFile(t, formal, ".stable/private", "private runtime", 0600)
	fixtureFile(t, formal, ".mewcode/settings", "private credentials", 0600)
	global := filepath.Join(t.TempDir(), "global-gitconfig")
	if err := os.WriteFile(global, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(formal, ".git"))
	t.Setenv("GIT_WORK_TREE", formal)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(formal, ".git/objects"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(formal, ".git/hooks"))
	t.Setenv("SSH_AUTH_SOCK", "/private/credential-agent")
	state, err := g.Materialize(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := layout.Paths(id)
	if !validGitOID(state.BaselineCommit) || state.UsedBytes <= 0 {
		t.Fatalf("missing materialization receipt: %+v", state)
	}
	formalManifest, err := BuildManifest(context.Background(), formal, Limits{})
	if err != nil || formalManifest.Digest != state.BaselineDigest {
		t.Fatalf("baseline lost source data: %+v, %v", formalManifest, err)
	}
	checkout, err := BuildManifest(context.Background(), paths.Checkout, Limits{})
	if err != nil || checkout.Digest != state.BaselineDigest {
		t.Fatalf("checkout differs from working data: %+v, %v", checkout, err)
	}
	for name, want := range map[string]string{".git/config": config, ".git/index": "source index bytes", ".git/HEAD": "ref: refs/heads/formal\n", ".git/hooks/post-commit": hook, ".stable/private": "private runtime", ".mewcode/settings": "private credentials"} {
		got, err := os.ReadFile(filepath.Join(formal, name))
		if err != nil || string(got) != want {
			t.Fatalf("formal metadata touched: %s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source hook/filter executed: %v", err)
	}
	if got := strings.TrimSpace(gitQuery(t, g, paths, paths.Repository, "", "rev-list", "--count", "--all")); got != "1" {
		t.Fatalf("formal history copied: commit count %q", got)
	}
	for name, want := range map[string]string{
		"dirty.txt":     "dirty source bytes\n",
		"untracked.txt": "untracked source bytes\n",
		"ignored.txt":   "ignored but preserved",
	} {
		if got := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":"+name); got != want {
			t.Fatalf("private baseline lost or changed %s bytes: %q", name, got)
		}
	}
	gitDir := filepath.Join(paths.Repository, "worktrees", id)
	index := gitQuery(t, g, paths, gitDir, paths.Checkout, "ls-files", "--stage", "-z")
	blob := "dirty source bytes\n"
	h := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(blob), blob)))
	if !strings.Contains(index, "100644 "+hex.EncodeToString(h[:])+" 0\tdirty.txt\x00") || !strings.Contains(index, "scripts/run.sh\x00") {
		t.Fatalf("linked private index was not initialized: %q", index)
	}
	for _, name := range []string{".stable", ".mewcode"} {
		if _, err := os.Lstat(filepath.Join(paths.Checkout, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source metadata copied: %s: %v", name, err)
		}
	}
	if _, err := g.Validate(context.Background(), scope, id); err != nil {
		t.Fatal(err)
	}
	// T4 leaves ready/outcome publication to the manager.
	record, err := store.Load(context.Background(), scope, id)
	if err != nil || record.Snapshot.State != StateCreating {
		t.Fatalf("engine published a false public ready fact: %+v, %v", record, err)
	}
	if _, err := g.Materialize(context.Background(), scope, id); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing repository reinitialized: %v", err)
	}
	fixtureFile(t, paths.Checkout, "dirty.txt", "workspace edit", 0640)
	if _, err := g.Validate(context.Background(), scope, id); err != nil {
		t.Fatalf("legitimate dirty checkout rejected: %v", err)
	}
}

func TestPrivateGitSiblingsDoNotShareRefsOrObjects(t *testing.T) {
	g, layout, store, scope, firstID := privateGitFixture(t)
	formal := layout.FormalRoot()
	fixtureFile(t, formal, "base.txt", "shared source baseline\n", 0600)
	secondID := "workspace-sibling"
	if _, err := store.Create(context.Background(), scope, secondID, "sibling", "create-sibling"); err != nil {
		t.Fatal(err)
	}

	firstState, err := g.Materialize(context.Background(), scope, firstID)
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := g.Materialize(context.Background(), scope, secondID)
	if err != nil {
		t.Fatal(err)
	}
	firstPaths, err := layout.Paths(firstID)
	if err != nil {
		t.Fatal(err)
	}
	secondPaths, err := layout.Paths(secondID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPaths.Repository == secondPaths.Repository || firstState.BaselineDigest != secondState.BaselineDigest {
		t.Fatalf("sibling private repositories or baseline digests are unexpected: first=%+v second=%+v", firstState, secondState)
	}

	commonDir := func(paths Paths) string {
		t.Helper()
		common := strings.TrimSpace(gitQuery(t, g, paths, paths.Repository, "", "rev-parse", "--git-common-dir"))
		if !filepath.IsAbs(common) {
			common = filepath.Join(paths.Root, common)
		}
		return filepath.Clean(common)
	}
	firstCommon, secondCommon := commonDir(firstPaths), commonDir(secondPaths)
	if firstCommon != filepath.Clean(firstPaths.Repository) || secondCommon != filepath.Clean(secondPaths.Repository) || firstCommon == secondCommon {
		t.Fatalf("sibling common dirs are not independently owned: first=%q second=%q", firstCommon, secondCommon)
	}

	fixtureFile(t, firstPaths.Checkout, "private-only.txt", "object unique to first workspace\n", 0600)
	objectID := strings.TrimSpace(gitQuery(t, g, firstPaths, firstPaths.Repository, firstPaths.Checkout, "hash-object", "-w", filepath.Join(firstPaths.Checkout, "private-only.txt")))
	if !validGitOID(objectID) {
		t.Fatalf("private-only object has invalid ID %q", objectID)
	}
	gitQuery(t, g, firstPaths, firstPaths.Repository, "", "update-ref", "refs/m09/private-only", objectID)
	if got := strings.TrimSpace(gitQuery(t, g, firstPaths, firstPaths.Repository, "", "rev-parse", "--verify", "refs/m09/private-only")); got != objectID {
		t.Fatalf("first private ref=%q, want %q", got, objectID)
	}
	if _, err := gitQueryResult(t, g, secondPaths, secondPaths.Repository, "", "rev-parse", "--verify", "refs/m09/private-only"); err == nil {
		t.Fatal("first workspace private ref appeared in sibling repository")
	}
	if _, err := gitQueryResult(t, g, secondPaths, secondPaths.Repository, "", "cat-file", "-e", objectID); err == nil {
		t.Fatal("first workspace private object appeared in sibling repository")
	}
}

func TestPrivateGitSourceChangeAndPartialRollback(t *testing.T) {
	for _, failure := range []string{"source-change", "cancel", "read-tree-failure"} {
		t.Run(failure, func(t *testing.T) {
			g, layout, store, scope, id := privateGitFixture(t)
			fixtureFile(t, layout.FormalRoot(), "data", "initial", 0600)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			realRun := g.runGit
			injected := errors.New("injected read-tree failure")
			g.runGit = func(ctx context.Context, invocation gitInvocation) ([]byte, error) {
				if failure == "cancel" && invocation.Args[0] == "fast-import" {
					cancel()
					return nil, ctx.Err()
				}
				if failure == "read-tree-failure" && invocation.Args[0] == "read-tree" {
					return nil, injected
				}
				out, err := realRun(ctx, invocation)
				if failure == "source-change" && err == nil && invocation.Args[0] == "fast-import" {
					fixtureFile(t, layout.FormalRoot(), "data", "changed during creation", 0600)
				}
				return out, err
			}
			_, err := g.Materialize(ctx, scope, id)
			want := error(ErrSourceChanged)
			if failure == "cancel" {
				want = context.Canceled
			}
			if failure == "read-tree-failure" {
				want = injected
			}
			if !errors.Is(err, want) {
				t.Fatalf("failure not propagated: %v, want %v", err, want)
			}
			paths, _ := layout.Paths(id)
			for _, name := range []string{"baseline", "repo.git", "checkout", "run", gitStateName} {
				if _, err := os.Lstat(filepath.Join(paths.Root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("known partial resource not rolled back: %s, %v", name, err)
				}
			}
			if _, err := store.Load(context.Background(), scope, id); err != nil {
				t.Fatalf("ownership intent lost on failure: %v", err)
			}
		})
	}
}

func TestPrivateGitRollbackPreservesUnknownReplacement(t *testing.T) {
	g, layout, _, scope, id := privateGitFixture(t)
	fixtureFile(t, layout.FormalRoot(), "data", "initial", 0600)
	paths, _ := layout.Paths(id)
	realRun := g.runGit
	injected := errors.New("injected replacement")
	g.runGit = func(ctx context.Context, invocation gitInvocation) ([]byte, error) {
		out, err := realRun(ctx, invocation)
		if err == nil && invocation.Args[0] == "fast-import" {
			if err := os.Rename(paths.Repository, paths.Repository+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(paths.Repository, 0700); err != nil {
				t.Fatal(err)
			}
			fixtureFile(t, paths.Repository, "unknown", "keep me", 0600)
			return nil, injected
		}
		return out, err
	}
	_, err := g.Materialize(context.Background(), scope, id)
	if !errors.Is(err, injected) || !errors.Is(err, ErrOwnership) {
		t.Fatalf("replacement rollback not blocked: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(paths.Repository, "unknown"))
	if err != nil || string(data) != "keep me" {
		t.Fatalf("unknown replacement removed: %q, %v", data, err)
	}
	if _, err := os.Stat(paths.Repository + "-old"); err != nil {
		t.Fatalf("replaced original was guessed away: %v", err)
	}
}

func TestPrivateGitValidationRejectsMetadataTampering(t *testing.T) {
	for _, attack := range []string{"pointer", "config-include", "alternates", "remote", "hardlink", "private-head", "protected-data"} {
		t.Run(attack, func(t *testing.T) {
			g, layout, _, scope, id := privateGitFixture(t)
			fixtureFile(t, layout.FormalRoot(), "data", "initial", 0600)
			if _, err := g.Materialize(context.Background(), scope, id); err != nil {
				t.Fatal(err)
			}
			paths, _ := layout.Paths(id)
			switch attack {
			case "pointer":
				fixtureFile(t, paths.Checkout, ".git", "gitdir: /other/project/.git\n", 0600)
			case "config-include":
				fixtureFile(t, paths.Repository, "config", privateGitConfiguration+"[include]\n path = /other/config\n", 0600)
			case "alternates":
				fixtureFile(t, paths.Repository, "objects/info/alternates", "/other/objects\n", 0600)
			case "remote":
				fixtureFile(t, paths.Repository, "refs/remotes/origin/main", strings.Repeat("0", 40)+"\n", 0600)
			case "hardlink":
				if err := os.Link(filepath.Join(paths.Repository, "HEAD"), filepath.Join(paths.Repository, "linked-HEAD")); err != nil {
					t.Fatal(err)
				}
			case "private-head":
				fixtureFile(t, paths.Repository, filepath.Join("worktrees", id, "HEAD"), strings.Repeat("0", 40)+"\n", 0600)
			case "protected-data":
				fixtureFile(t, paths.Checkout, ".stable/injected", "private injection", 0600)
			}
			if _, err := g.Validate(context.Background(), scope, id); err == nil {
				t.Fatalf("unsafe metadata accepted: %s", attack)
			}
		})
	}
}

func TestPrivateGitEnvironmentQuotingAndOutputBound(t *testing.T) {
	paths := Paths{Root: "/state/project/id", Run: "/state/project/id/run", Repository: "/state/project/id/repo.git"}
	env := sanitizedGitEnvironment(paths)
	for _, key := range []string{"GIT_CONFIG_COUNT=", "GIT_DIR=", "GIT_WORK_TREE=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=", "SSH_AUTH_SOCK=", "HTTP_PROXY=", "GIT_EXEC_PATH="} {
		for _, value := range env {
			if strings.HasPrefix(value, key) {
				t.Fatalf("unsafe environment key inherited: %q", value)
			}
		}
	}
	if quoteGitPath("a\n\"\\界") != `"a\012\"\\\347\225\214"` {
		t.Fatalf("incorrect Git C quoting: %q", quoteGitPath("a\n\"\\界"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &gitOutput{limit: 4, cancel: cancel}
	if n, err := output.Write([]byte("safe")); n != 4 || err != nil {
		t.Fatalf("valid output rejected: %d, %v", n, err)
	}
	if _, err := output.Write([]byte("overflow")); !errors.Is(err, ErrQuota) || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("output cap did not cancel: %v, %v", err, ctx.Err())
	}
	if !reflect.DeepEqual(output.buffer.Bytes(), []byte("safe")) {
		t.Fatal("oversized output retained")
	}
}

func TestPrivateGitImportStreamPreservesBytesAndModes(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "binary", string([]byte{0, 1, 2, '\n'}), 0600)
	fixtureFile(t, root, "script", "run", 0755)
	manifest, err := BuildManifest(context.Background(), root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := writeImport(context.Background(), &stream, root, manifest); err != nil {
		t.Fatal(err)
	}
	data := stream.String()
	if !strings.Contains(data, "data 4\n"+string([]byte{0, 1, 2, '\n'})) || !strings.Contains(data, "M 100755 :2 \"script\"\n") || !strings.HasSuffix(data, "\ndone\n") {
		t.Fatalf("invalid import protocol: %q", data)
	}
	fixtureFile(t, root, "binary", "modified", 0600)
	if err := writeImport(context.Background(), io.Discard, root, manifest); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed baseline imported: %v", err)
	}
}
