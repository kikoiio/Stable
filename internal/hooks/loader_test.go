package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHookFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFilesMergeAndOverride(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	project := filepath.Join(dir, "project.yaml")
	writeHookFile(t, user, `
hooks:
  - id: shared
    event: run_start
    action: {type: prompt, message: user}
  - event: run_end
    action: {type: prompt, message: user-end}
`)
	writeHookFile(t, project, `
hooks:
  - id: shared
    event: run_start
    action: {type: prompt, message: project}
  - event: pre_tool_use
    action: {type: prompt, message: project-pre}
`)
	got := LoadFiles(user, project)
	if len(got.Hooks) != 3 {
		t.Fatalf("hooks=%d rejections=%v", len(got.Hooks), got.Rejections)
	}
	if got.Hooks[0].ID != "user:2" || got.Hooks[0].Source != "user" {
		t.Fatalf("expected remaining user auto-id first: %+v", got.Hooks[0])
	}
	if got.Hooks[1].ID != "shared" || got.Hooks[1].Source != "project" || got.Hooks[1].Action.Message != "project" {
		t.Fatalf("expected project override: %+v", got.Hooks[1])
	}
	if got.Hooks[2].ID != "project:2" {
		t.Fatalf("expected project auto-id: %+v", got.Hooks[2])
	}
	joined := strings.Join(got.Rejections, "\n")
	if !strings.Contains(joined, "project overrides user") {
		t.Fatalf("missing override rejection: %v", got.Rejections)
	}
}

func TestLoadFilesRejectsDuplicatesSymlinkBadYAMLAndMissing(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	writeHookFile(t, user, `
hooks:
  - id: a
    event: run_start
    action: {type: prompt, message: one}
  - id: a
    event: run_end
    action: {type: prompt, message: two}
  - event: nope
    action: {type: prompt, message: bad}
`)
	got := LoadFiles(user, filepath.Join(dir, "missing.yaml"))
	if len(got.Hooks) != 1 || got.Hooks[0].ID != "a" {
		t.Fatalf("expected one valid hook, got %+v rejections=%v", got.Hooks, got.Rejections)
	}
	joined := strings.Join(got.Rejections, "\n")
	if !strings.Contains(joined, "duplicate id a") || !strings.Contains(joined, "event") {
		t.Fatalf("rejections incomplete: %v", got.Rejections)
	}

	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(user, link); err != nil {
		t.Fatal(err)
	}
	got = LoadFiles(link, "")
	if len(got.Hooks) != 0 || !strings.Contains(strings.Join(got.Rejections, "\n"), "symbolic link") {
		t.Fatalf("symlink not refused: %+v", got)
	}

	bad := filepath.Join(dir, "bad.yaml")
	writeHookFile(t, bad, ": not yaml")
	got = LoadFiles(bad, "")
	if len(got.Hooks) != 0 || !strings.Contains(strings.Join(got.Rejections, "\n"), "invalid YAML") {
		t.Fatalf("bad yaml not refused: %+v", got)
	}

	got = LoadFiles("", "")
	if len(got.Hooks) != 0 || len(got.Rejections) != 0 {
		t.Fatalf("empty load should be silent: %+v", got)
	}
}
