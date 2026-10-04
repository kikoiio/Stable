package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryListsToolsAndProviderSchemasByName(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&GrepTool{})
	reg.Register(&ReadFileTool{})
	reg.Register(&GlobTool{})
	tools := reg.ListTools()
	if got := tools[0].Name(); got != "Glob" {
		t.Fatalf("first tool = %q, want Glob", got)
	}
	if got := tools[1].Name(); got != "Grep" {
		t.Fatalf("second tool = %q, want Grep", got)
	}

	schemas := reg.GetAllSchemas()
	gotNames := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		gotNames = append(gotNames, schema["name"].(string))
	}
	wantNames := []string{"command", "glob", "grep", "read_file"}
	if len(gotNames) != len(wantNames) {
		t.Fatalf("schema names = %v, want %v", gotNames, wantNames)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("schema names = %v, want %v", gotNames, wantNames)
		}
	}
	if got := schemas[2]["name"]; got != "grep" {
		t.Fatalf("grep schema name = %v, want grep", got)
	}
	if got := schemas[3]["name"]; got != "read_file" {
		t.Fatalf("read schema name = %v, want read_file", got)
	}
}

func TestProviderSchemasMatchExecutorDispatch(t *testing.T) {
	reg := CreateDefaultRegistry()
	seen := make(map[string]bool)
	for _, schema := range reg.GetAllSchemas() {
		name := schema["name"].(string)
		seen[name] = true
		input := schema["input_schema"].(map[string]any)
		if input["type"] != "object" {
			t.Fatalf("%s input schema type = %v, want object", name, input["type"])
		}
	}
	for _, name := range []string{"read_file", "glob", "grep", "write_file", "edit_file", "command"} {
		if !seen[name] {
			t.Fatalf("provider schemas missing %q", name)
		}
	}
	command := CommandSchema()["input_schema"].(map[string]any)
	properties := command["properties"].(map[string]any)
	timeout := properties["timeout"].(map[string]any)
	if timeout["type"] != "integer" || timeout["default"] != 120 || timeout["maximum"] != 600 {
		t.Fatalf("command timeout schema = %#v", timeout)
	}
}

func TestReadFileNumberingAndInvalidRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFileTool{}
	res := tool.Execute(context.Background(), map[string]any{"file_path": path, "offset": 1, "limit": 2})
	if res.IsError || res.Output != "2\ttwo\n3\tthree" {
		t.Fatalf("read result = %#v", res)
	}
	for _, args := range []map[string]any{
		{"file_path": path, "offset": -1},
		{"file_path": path, "limit": -1},
	} {
		if res := tool.Execute(context.Background(), args); !res.IsError {
			t.Fatalf("invalid range should fail: %#v", args)
		}
	}
}

func TestReadFileEmptyAndDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if res := (&ReadFileTool{}).Execute(context.Background(), map[string]any{"file_path": empty}); res.IsError {
		t.Fatalf("empty file result = %#v", res)
	}
	if res := (&ReadFileTool{}).Execute(context.Background(), map[string]any{"file_path": root}); !res.IsError {
		t.Fatal("directory read should fail")
	}
}

func TestGrepSkipsDirectoriesAndFiltersFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"a.go":        "needle\nother",
		"b.txt":       "needle",
		".git/skip":   "needle",
		"nested/c.go": "needle",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := (&GrepTool{}).Execute(context.Background(), map[string]any{"path": root, "pattern": "needle", "include": "*.go"})
	if res.IsError || strings.Contains(res.Output, ".git") || strings.Contains(res.Output, "b.txt") || !strings.Contains(res.Output, "a.go:1:needle") || !strings.Contains(res.Output, "nested/c.go:1:needle") {
		t.Fatalf("grep result = %#v", res)
	}
}

func TestGlobSortsByModificationTime(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.go")
	newPath := filepath.Join(root, "new.go")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	res := (&GlobTool{}).Execute(context.Background(), map[string]any{"path": root, "pattern": "*.go"})
	if res.IsError || !strings.HasPrefix(res.Output, "new.go\nold.go") {
		t.Fatalf("glob result = %#v", res)
	}
}

func TestWriteAndEditRequireReadAndReturnDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("before\nkeep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewFileStateCache()
	write := &WriteFileTool{FileStateCache: cache}
	edit := &EditFileTool{FileStateCache: cache}
	if res := write.Execute(context.Background(), map[string]any{"file_path": path, "content": "after\nkeep\n"}); !res.IsError {
		t.Fatal("overwrite without read should fail")
	}
	(&ReadFileTool{FileStateCache: cache}).Execute(context.Background(), map[string]any{"file_path": path})
	res := edit.Execute(context.Background(), map[string]any{"file_path": path, "old_string": "before", "new_string": "after"})
	if res.IsError || !strings.Contains(res.Output, "addition") || !strings.Contains(res.Output, "removal") {
		t.Fatalf("edit result = %#v", res)
	}
	if got, _ := os.ReadFile(path); string(got) != "after\nkeep\n" {
		t.Fatalf("edited content = %q", got)
	}
}

func TestBuildDiffHandlesSeparatedChanges(t *testing.T) {
	res := BuildDiff("a\nb\nc\nd\ne\nf\n", "a\nB\nc\nd\nE\nf\n")
	if res.Additions != 2 || res.Removals != 2 || !strings.Contains(res.Text, "-    2  b") || !strings.Contains(res.Text, "+    5  E") {
		t.Fatalf("diff = %#v", res)
	}
}

func TestSafeCommandRejectsComposition(t *testing.T) {
	if !IsSafeCommand("git status") || IsSafeCommand("git status | head") || IsSafeCommand("echo hi > out") || IsSafeCommand("pwd; rm -rf .") {
		t.Fatal("safe command classification is incorrect")
	}
}
