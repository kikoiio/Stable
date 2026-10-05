package todo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/redact"
)

func TestStorePathAndPermissions(t *testing.T) {
	root := t.TempDir()
	tl := NewTaskList(root, "session-a", nil)
	first := mustCreate(t, tl, "one")
	second := mustCreate(t, tl, "two")
	if first.ID == second.ID {
		t.Fatal("task ids are not unique")
	}

	for _, check := range []struct {
		name string
		path string
	}{
		{"state dir", filepath.Join(root, ".stable")},
		{"tasks dir", filepath.Join(root, ".stable", "tasks")},
	} {
		st, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0700 {
			t.Fatalf("%s mode %o", check.name, st.Mode().Perm())
		}
	}
	path := filepath.Join(root, ".stable", "tasks", "session-a.json")
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		t.Fatalf("tasks file mode %v", st.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"subject": "one"`) {
		t.Fatalf("unexpected persisted content: %s", data)
	}
}

func TestSaveRedactsCredentials(t *testing.T) {
	root := t.TempDir()
	secret := "supersecrettokenvalue"
	tl := NewTaskList(root, "session-a", nil)
	tl.SetCredentials([]string{secret})

	created, err := tl.Create("token is "+secret, "note "+secret, "form "+secret, map[string]string{"note": secret})
	if err != nil {
		t.Fatal(err)
	}
	if created.Subject == redact.Placeholder {
		t.Fatal("returned task should keep the caller's text")
	}

	data, err := os.ReadFile(filepath.Join(root, ".stable", "tasks", "session-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("credential leaked to disk: %s", data)
	}
	if !strings.Contains(string(data), redact.Placeholder) {
		t.Fatalf("placeholder missing: %s", data)
	}

	reopened := NewTaskList(root, "session-a", nil)
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "token is "+redact.Placeholder || got.Metadata["note"] != redact.Placeholder {
		t.Fatalf("reload not redacted: %+v", got)
	}
}

func TestShortCredentialRefusesWrite(t *testing.T) {
	root := t.TempDir()
	tl := NewTaskList(root, "session-a", nil)
	seed := mustCreate(t, tl, "seed")
	path := filepath.Join(root, ".stable", "tasks", "session-a.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	tl.SetCredentials([]string{"short"})
	if _, err := tl.Create("another", "d", "", nil); err == nil {
		t.Fatal("write with unusable credential accepted")
	}
	if _, err := tl.Update(seed.ID, UpdatePatch{AddBlocks: []string{seed.ID}}); err == nil {
		t.Fatal("update with unusable credential accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("refused write still modified the file:\n%s\n%s", before, after)
	}
}

func TestLoadAndSaveRejectSymlinkedFile(t *testing.T) {
	root := t.TempDir()
	tl := NewTaskList(root, "session-a", nil)
	mustCreate(t, tl, "seed")
	path := filepath.Join(root, ".stable", "tasks", "session-a.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.List(); err == nil {
		t.Fatal("load accepted a symlinked tasks file")
	}
	if _, err := tl.Create("another", "d", "", nil); err == nil {
		t.Fatal("save accepted a symlinked tasks file")
	}
}

func TestSaveRejectsSymlinkedDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// .stable itself is a symlink.
	tl := NewTaskList(root, "session-a", nil)
	if err := os.Symlink(outside, filepath.Join(root, ".stable")); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Create("subject", "d", "", nil); err == nil {
		t.Fatal("save accepted a symlinked state directory")
	}
	if err := os.Remove(filepath.Join(root, ".stable")); err != nil {
		t.Fatal(err)
	}

	// Only the tasks directory is a symlink.
	if err := os.MkdirAll(filepath.Join(root, ".stable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".stable", "tasks")); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Create("subject", "d", "", nil); err == nil {
		t.Fatal("save accepted a symlinked tasks directory")
	}
}

func TestNewStoreValidation(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "a/b", `a\b`, "..", "../escape", "a/../b"} {
		if _, err := NewStore(root, id); err == nil {
			t.Fatalf("NewStore accepted session id %q", id)
		}
	}
	if _, err := NewStore("", "session-a"); err == nil {
		t.Fatal("NewStore accepted an empty project root")
	}
	if _, err := NewStore("   ", "session-a"); err == nil {
		t.Fatal("NewStore accepted a blank project root")
	}
	store, err := NewStore(root, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".stable", "tasks", "session-a.json"); store.path != want {
		t.Fatalf("store path %q want %q", store.path, want)
	}
}

func TestSchemaConstantsShape(t *testing.T) {
	for _, schema := range []map[string]any{TaskCreateSchema, TaskGetSchema, TaskListSchema, TaskUpdateSchema} {
		name, _ := schema["name"].(string)
		if !strings.HasPrefix(name, "task_") {
			t.Fatalf("schema name %q", name)
		}
		if _, ok := schema["description"].(string); !ok {
			t.Fatalf("%s lacks a description", name)
		}
		input, ok := schema["input_schema"].(map[string]any)
		if !ok || input["type"] != "object" {
			t.Fatalf("%s input_schema malformed", name)
		}
		if _, ok := input["properties"].(map[string]any); !ok {
			t.Fatalf("%s lacks properties", name)
		}
	}
	if status, ok := TaskUpdateSchema["input_schema"].(map[string]any)["properties"].(map[string]any)["status"].(map[string]any); !ok {
		t.Fatal("task_update lacks a status property")
	} else if enum, ok := status["enum"].([]string); !ok || len(enum) != 4 {
		t.Fatalf("task_update status enum: %v", status["enum"])
	}
	if required, ok := TaskCreateSchema["input_schema"].(map[string]any)["required"].([]string); !ok ||
		len(required) != 2 || required[0] != "subject" || required[1] != "description" {
		t.Fatalf("task_create required: %v", required)
	}
	if required, ok := TaskUpdateSchema["input_schema"].(map[string]any)["required"].([]string); !ok ||
		len(required) != 1 || required[0] != "taskId" {
		t.Fatalf("task_update required: %v", required)
	}
}
