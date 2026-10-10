//go:build linux || darwin

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateGitMaterializePreservesNestedIgnoredDirectoryFiles(t *testing.T) {
	g, layout, _, scope, id := privateGitFixture(t)
	formal := layout.FormalRoot()
	sourceGit(t, formal, "init", "--initial-branch=main")
	fixtureFile(t, formal, ".gitignore", "ignored-tree/\n", 0600)
	fixtureFile(t, formal, "ignored-tree/nested/payload.bin", "ignored nested bytes\x00\x01", 0640)
	fixtureFile(t, formal, "ignored-tree/nested/run.sh", "#!/bin/sh\nexit 0\n", 0755)
	if status := sourceGit(t, formal, "status", "--ignored", "--porcelain=v1"); !strings.Contains(status, "!! ignored-tree/") {
		t.Fatalf("fixture directory is not ignored: %q", status)
	}

	state, err := g.Materialize(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := BuildManifest(context.Background(), formal, Limits{})
	if err != nil || formalManifest.Digest != state.BaselineDigest {
		t.Fatalf("private baseline digest does not cover the complete ignored source tree: manifest=%+v state=%+v err=%v", formalManifest, state, err)
	}
	checkoutManifest, err := BuildManifest(context.Background(), paths.Checkout, Limits{})
	if err != nil || checkoutManifest.Digest != state.BaselineDigest {
		t.Fatalf("checkout omitted nested ignored files: manifest=%+v state=%+v err=%v", checkoutManifest, state, err)
	}
	entries := make(map[string]ManifestEntry, len(checkoutManifest.Entries))
	for _, entry := range checkoutManifest.Entries {
		entries[entry.Path] = entry
	}
	for path, want := range map[string]struct {
		bytes string
		mode  uint32
	}{
		"ignored-tree/nested/payload.bin": {bytes: "ignored nested bytes\x00\x01", mode: 0640},
		"ignored-tree/nested/run.sh":      {bytes: "#!/bin/sh\nexit 0\n", mode: 0755},
	} {
		entry, ok := entries[path]
		if !ok || entry.Mode != want.mode {
			t.Fatalf("nested ignored entry missing or changed mode: path=%q entry=%+v found=%t", path, entry, ok)
		}
		got, err := os.ReadFile(filepath.Join(paths.Checkout, filepath.FromSlash(path)))
		if err != nil || string(got) != want.bytes {
			t.Fatalf("checkout data for %s=%q err=%v want=%q", path, got, err, want.bytes)
		}
		blob := gitQuery(t, g, paths, paths.Repository, "", "cat-file", "blob", state.BaselineCommit+":"+path)
		if blob != want.bytes {
			t.Fatalf("private Git baseline data for %s=%q want=%q", path, blob, want.bytes)
		}
	}
}
