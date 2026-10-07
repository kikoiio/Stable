package agentcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func writeDefinition(t *testing.T, path, name, body string) {
	t.Helper()
	if err := os.WriteFile(path, definitionText(name, "", body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPrecedenceReloadAndImmutableViews(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	writeDefinition(t, filepath.Join(user, "explore.md"), "explore", "User body")
	projectFile := filepath.Join(project, "explore.md")
	writeDefinition(t, projectFile, "explore", "Project body")
	writeDefinition(t, filepath.Join(user, "user.md"), "user-role", "User role body")
	c := New(user, project)
	old, ok := c.Resolve("explore")
	if !ok || old.Source != "project" || old.Instruction != "Project body" {
		t.Fatalf("bad precedence: %#v", old)
	}
	meta := c.Snapshot()
	names := make([]string, 0, len(meta.Definitions))
	for _, d := range meta.Definitions {
		names = append(names, d.Name)
		if !d.ReadOnly {
			t.Fatal("non-readonly role")
		}
	}
	if !reflect.DeepEqual(names, []string{"explore", "general-purpose", "plan", "user-role"}) {
		t.Fatalf("bad sorted inventory: %v", names)
	}
	data, _ := json.Marshal(meta)
	if strings.Contains(string(data), "body") || strings.Contains(string(data), "instruction") {
		t.Fatalf("inventory leaked instruction: %s", data)
	}
	meta.Definitions[0].Tools[0] = "command"
	if c.Snapshot().Definitions[0].Tools[0] == "command" {
		t.Fatal("snapshot shares internal tool slice")
	}
	writeDefinition(t, projectFile, "explore", "New project body")
	c.Reload()
	updated, _ := c.Resolve("explore")
	if updated.Instruction != "New project body" || old.Instruction != "Project body" {
		t.Fatal("reload changed previous definition or failed to update")
	}
	if err := os.Remove(projectFile); err != nil {
		t.Fatal(err)
	}
	c.Reload()
	updated, _ = c.Resolve("explore")
	if updated.Source != "user" {
		t.Fatalf("deletion retained project definition: %#v", updated)
	}
	if err := os.WriteFile(filepath.Join(user, "explore.md"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if len(c.Reload().Rejections) != 1 {
		t.Fatal("invalidated file not rejected")
	}
	updated, _ = c.Resolve("explore")
	if updated.Source != "builtin" {
		t.Fatalf("invalid reload retained user definition: %#v", updated)
	}
	if err := os.Remove(filepath.Join(user, "user.md")); err != nil {
		t.Fatal(err)
	}
	c.Reload()
	if _, ok := c.Resolve("user-role"); ok {
		t.Fatal("deleted custom role remained active")
	}
}

func TestResolvedToolRulesAreCopied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.md"), definitionText("test", "tools: [grep, read_file]\ndisallowedTools: [read_file]\n", "Instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(dir, "")
	d, _ := c.Resolve("test")
	d.Tools[0] = "command"
	d.DisallowedTools[0] = "grep"
	again, _ := c.Resolve("test")
	if !reflect.DeepEqual(again.EffectiveTools(), []string{"grep"}) {
		t.Fatal("resolved definition shares mutable slices")
	}
}

func TestInvalidEntriesAndSymlinksAreRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires symlink creation privilege")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.md")
	writeDefinition(t, outside, "outside", "Private external body")
	writeDefinition(t, filepath.Join(dir, "valid.md"), "valid", "Valid body")
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "linked-directory")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("---\npermissionMode: bypassPermissions\n---\nPrivate invalid body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.md"), []byte(strings.Repeat("x", MaxFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(dir, "")
	if _, ok := c.Resolve("valid"); !ok {
		t.Fatal("valid sibling blocked by invalid files")
	}
	if _, ok := c.Resolve("outside"); ok {
		t.Fatal("symlink target loaded")
	}
	if len(c.Snapshot().Rejections) != 4 {
		t.Fatalf("missing rejection reasons: %v", c.Snapshot().Rejections)
	}
	rootLink := filepath.Join(t.TempDir(), "agents")
	if err := os.Symlink(dir, rootLink); err != nil {
		t.Fatal(err)
	}
	linked := New(rootLink, "").Snapshot()
	if len(linked.Definitions) != 3 || len(linked.Rejections) != 1 {
		t.Fatalf("linked directory loaded: %#v", linked)
	}
	ancestorLink := filepath.Join(t.TempDir(), "stable")
	if err := os.Symlink(filepath.Dir(dir), ancestorLink); err != nil {
		t.Fatal(err)
	}
	ancestor := New(filepath.Join(ancestorLink, filepath.Base(dir)), "").Snapshot()
	if len(ancestor.Definitions) != 3 || len(ancestor.Rejections) != 1 {
		t.Fatalf("linked ancestor loaded: %#v", ancestor)
	}
}

func TestMissingDirectoriesAndConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "missing"), "")
	if got := c.Snapshot(); len(got.Definitions) != 3 || len(got.Rejections) != 0 {
		t.Fatalf("missing directory: %#v", got)
	}
	c = New(dir, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if j%3 == 0 {
					c.Reload()
				}
				s := c.Snapshot()
				if len(s.Definitions) != 3 {
					t.Error("partial snapshot")
				}
				d, ok := c.Resolve("explore")
				if !ok || d.Instruction == "" {
					t.Error("incomplete resolved definition")
				}
			}
		}()
	}
	wg.Wait()
}
