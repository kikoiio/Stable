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
