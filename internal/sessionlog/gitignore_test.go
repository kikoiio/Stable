package sessionlog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareAddsLocalGitExcludeIdempotently(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if _, err := Prepare(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".stable", "sessions", "placeholder.jsonl")
	if err := exec.Command("git", "-C", root, "check-ignore", "-q", path).Run(); err != nil {
		t.Fatalf("session path not ignored: %v", err)
	}
	data, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatal(err)
	}
	info := strings.TrimSpace(string(data))
	if !filepath.IsAbs(info) {
		info = filepath.Join(root, info)
	}
	b, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(b), ".stable/sessions/"); count != 1 {
		t.Fatalf("exclude rule count=%d contents=%q", count, b)
	}
}
