package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandCompletion(t *testing.T) {
	got := builtinCommands{}.List("/g")
	if len(got) != 1 || got[0].InsertText != "/goals" {
		t.Fatalf("unexpected commands: %+v", got)
	}
	if len(builtinCommands{}.List("/unknown")) != 0 {
		t.Fatal("unknown command appeared")
	}
}

func TestCompletionOverlayClipsToAvailableArea(t *testing.T) {
	items := []CompletionItem{{Label: "/sessions"}, {Label: "/goals"}, {Label: "/very-long-command"}}
	out := (Overlay{Width: 8, Height: 2}).Render(items, 2)
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "/very…") {
		t.Fatalf("overlay did not clip: %q", out)
	}
}

func TestTabCompletionInsertsOnlyCommandOrRelativePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("private content"), 0600); err != nil {
		t.Fatal(err)
	}
	m := New("", root)
	m.Composer.SetValue("/g")
	updated, _ := m.handleChatKey(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.Composer.Value() != "/goals" {
		t.Fatalf("command completion=%q", m.Composer.Value())
	}
	updated, _ = m.handleChatKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.Navigation.Mode != GoalPickerView {
		t.Fatal("completed command did not open goal navigation")
	}
	m.Navigation.Mode = ChatView
	m.Composer.SetValue("@r")
	updated, _ = m.handleChatKey(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.Composer.Value() != "@readme.md" {
		t.Fatalf("path completion=%q", m.Composer.Value())
	}
	if strings.Contains(m.Composer.Value(), "private content") {
		t.Fatal("completion read file content")
	}
}
func TestPathCompletionStaysWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	got, err := (projectPathCompleter{}).Complete(root, "@r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InsertText != "@readme.md" {
		t.Fatalf("unexpected paths: %+v", got)
	}
	escaped, err := (projectPathCompleter{}).Complete(root, "@escape/")
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped) != 0 {
		t.Fatalf("root escape candidates: %+v", escaped)
	}
	parent, err := (projectPathCompleter{}).Complete(root, "@../")
	if err != nil {
		t.Fatal(err)
	}
	if len(parent) != 0 {
		t.Fatalf("parent traversal candidates: %+v", parent)
	}
}
