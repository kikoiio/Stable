package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/platform/secfile"
)

func TestScopeForType(t *testing.T) {
	tests := []struct {
		typeName MemoryType
		want     MemoryScope
		ok       bool
	}{
		{typeName: TypeUser, want: ScopeUser, ok: true},
		{typeName: TypeFeedback, want: ScopeUser, ok: true},
		{typeName: TypeProject, want: ScopeProject, ok: true},
		{typeName: TypeReference, want: ScopeProject, ok: true},
		{typeName: "unknown"},
	}
	for _, tt := range tests {
		got, ok := ScopeForType(tt.typeName)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ScopeForType(%q) = (%q, %v), want (%q, %v)", tt.typeName, got, ok, tt.want, tt.ok)
		}
	}
}

func TestStoreSaveListReadAndScopeRouting(t *testing.T) {
	project := t.TempDir()
	userConfig := t.TempDir()
	store, err := NewStore(project, userConfig)
	if err != nil {
		t.Fatal(err)
	}
	changes := []MemoryChange{
		{Action: ActionUpsert, Scope: ScopeUser, Type: TypeUser, Name: "Writing style", Description: "Use concise prose", Body: "Prefer short direct answers."},
		{Action: ActionUpsert, Scope: ScopeUser, Type: TypeFeedback, Name: "Feedback", Body: "Avoid long introductions."},
		{Action: ActionUpsert, Scope: ScopeProject, Type: TypeProject, Name: "Project", Body: "Uses Go."},
		{Action: ActionUpsert, Scope: ScopeProject, Type: TypeReference, Name: "Reference", Body: "See the local protocol notes."},
	}
	for _, change := range changes {
		if _, err := store.Save(change); err != nil {
			t.Fatalf("Save(%s/%s): %v", change.Scope, change.Name, err)
		}
	}
	entries, issues, err := store.List()
	if err != nil || len(issues) != 0 || len(entries) != 4 {
		t.Fatalf("List() = %d entries, issues %v, error %v", len(entries), issues, err)
	}
	for _, header := range entries {
		want, ok := ScopeForType(header.Type)
		if !ok || header.Scope != want {
			t.Errorf("type %q stored in scope %q, want %q", header.Type, header.Scope, want)
		}
		got, err := store.Read(header.Scope, header.Filename)
		if err != nil || got.Body == "" || got.Name != header.Name {
			t.Errorf("Read(%s): entry=%+v, err=%v", header.Filename, got, err)
		}
	}
	userInfo, err := os.Stat(filepath.Join(userConfig, "stable", "memory"))
	if err != nil || userInfo.Mode().Perm() != 0700 {
		t.Fatalf("user memory dir mode = %v, %v", userInfo, err)
	}
	userIndex, _, _, err := store.Index(ScopeUser)
	if err != nil || !strings.Contains(userIndex, "Writing style") || !strings.Contains(userIndex, "Feedback") {
		t.Fatalf("user index = %q, err = %v", userIndex, err)
	}
	projectIndex, _, _, err := store.Index(ScopeProject)
	if err != nil || !strings.Contains(projectIndex, "Project") || !strings.Contains(projectIndex, "Reference") {
		t.Fatalf("project index = %q, err = %v", projectIndex, err)
	}
}

func TestStoreRejectsScopeMismatchAndUnsafeRead(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(MemoryChange{Scope: ScopeProject, Type: TypeUser, Name: "wrong", Body: "no"}); err == nil {
		t.Fatal("scope/type mismatch was accepted")
	}
	if _, err := store.Read(ScopeProject, "../outside.md"); !errors.Is(err, secfile.ErrUnsafePath) {
		t.Fatal("unsafe filename was accepted")
	}
}

func TestStoreSkipsSymlinkAndBadFrontmatter(t *testing.T) {
	project := t.TempDir()
	store, err := NewStore(project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".stable", "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(project, ".stable", "memory", "bad.md")
	if err := os.WriteFile(bad, []byte("---\nscope: project\ntype: unknown\nname: Bad\nupdated_at: 2026-01-01T00:00:00Z\n---\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, ".stable", "memory", "link.md")); err != nil {
		t.Fatal(err)
	}
	headers, issues, err := store.Headers(ScopeProject)
	if err != nil || len(headers) != 0 || len(issues) != 2 {
		t.Fatalf("Headers() = %v, %v, %v; want no entries and two issues", headers, issues, err)
	}
}

func TestStoreUpdatesByNameAndClearsScope(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	change := MemoryChange{Action: ActionUpsert, Scope: ScopeProject, Type: TypeProject, Name: "Runtime", Body: "first"}
	first, err := store.Save(change)
	if err != nil {
		t.Fatal(err)
	}
	change.Body = "updated"
	second, err := store.Save(change)
	if err != nil {
		t.Fatal(err)
	}
	if first.Filename != second.Filename {
		t.Fatalf("update changed filename from %q to %q", first.Filename, second.Filename)
	}
	entry, err := store.Read(ScopeProject, second.Filename)
	if err != nil || entry.Body != "updated" {
		t.Fatalf("updated memory = %+v, %v", entry, err)
	}
	count, err := store.Clear(ScopeProject)
	if err != nil || count != 1 {
		t.Fatalf("Clear(project) = %d, %v", count, err)
	}
	entries, _, err := store.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("List after clear = %v, %v", entries, err)
	}
	if count, err := store.Clear(ScopeUser); err != nil || count != 0 {
		t.Fatalf("Clear(empty user scope) = %d, %v", count, err)
	}
}
