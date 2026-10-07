package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryEntryCountFileAndIndexLimits(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	store, err := NewStore(project, user)
	if err != nil {
		t.Fatal(err)
	}
	memoryDir := filepath.Join(project, ".stable", "memory")
	if err = os.MkdirAll(memoryDir, 0700); err != nil {
		t.Fatal(err)
	}
	frontmatter := func(name string) string {
		return fmt.Sprintf("---\nscope: project\ntype: project\nname: %s\ndescription: %s\nupdated_at: %s\n---\nbody\n", name, strings.Repeat("d", 256), time.Now().UTC().Format(time.RFC3339Nano))
	}
	for i := 0; i < MaxMemoryFiles+1; i++ {
		name := fmt.Sprintf("entry-%03d.md", i)
		if err = os.WriteFile(filepath.Join(memoryDir, name), []byte(frontmatter(name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	headers, issues, err := store.Headers(ScopeProject)
	if err != nil || len(headers) != MaxMemoryFiles || len(issues) == 0 {
		t.Fatalf("entry limit: headers=%d issues=%v err=%v", len(headers), issues, err)
	}
	if err = store.rebuildIndex(ScopeProject); err != nil {
		t.Fatal(err)
	}
	index, truncated, _, err := store.Index(ScopeProject)
	if err != nil || !truncated || len(index) > MaxMemoryIndexByte || len(strings.Split(index, "\n")) > MaxMemoryIndexLine {
		t.Fatalf("index bounds: bytes=%d lines=%d truncated=%v err=%v", len(index), len(strings.Split(index, "\n")), truncated, err)
	}
	if err = os.WriteFile(filepath.Join(memoryDir, "huge.md"), []byte(frontmatter("huge")+strings.Repeat("x", MaxMemoryFileBytes)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(ScopeProject, "huge.md"); err == nil {
		t.Fatal("oversized memory entry was read")
	}
	badFrontmatter := "---\n" + strings.Repeat("x: y\n", MaxFrontmatterLine) + "---\nbody\n"
	if err = os.WriteFile(filepath.Join(memoryDir, "bad.md"), []byte(badFrontmatter), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(ScopeProject, "bad.md"); err == nil {
		t.Fatal("frontmatter exceeding 30 lines was read")
	}
}

func TestMemoryIndexReadEnforcesByteAndLineBounds(t *testing.T) {
	project, user := t.TempDir(), t.TempDir()
	store, err := NewStore(project, user)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(project, ".stable", "memory")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lines := strings.Repeat("line\n", MaxMemoryIndexLine+10)
	if err = os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Repeat("x", MaxMemoryIndexByte+50)+lines), 0600); err != nil {
		t.Fatal(err)
	}
	index, truncated, _, err := store.Index(ScopeProject)
	if err != nil || !truncated || len(index) > MaxMemoryIndexByte || len(strings.Split(index, "\n")) > MaxMemoryIndexLine {
		t.Fatalf("read index bounds: bytes=%d lines=%d truncated=%v err=%v", len(index), len(strings.Split(index, "\n")), truncated, err)
	}
}
