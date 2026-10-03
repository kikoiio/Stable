package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

func TestComposerHeightAndDraft(t *testing.T) {
	c := NewComposer()
	c.SetSize(40, 4)
	c.SetValue("one\ntwo\nthree")
	if c.Height() != 3 {
		t.Fatalf("height=%d", c.Height())
	}
	c.SetSize(40, 2)
	if c.Height() != 2 {
		t.Fatalf("height did not clamp: %d", c.Height())
	}
	if !strings.Contains(c.Value(), "two") {
		t.Fatal("resize discarded draft")
	}
}

func TestComposerCtrlJAndSubmitIntent(t *testing.T) {
	m := New("", t.TempDir())
	update := func(k tea.KeyMsg) { got, _ := m.handleChatKey(k); m = got.(Model) }
	update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if got := m.Composer.Value(); got != "a\nb" {
		t.Fatalf("multiline draft=%q", got)
	}
}

func TestComposerSubmitClearsOnlyAfterSubmit(t *testing.T) {
	m := New("/definitely/missing/socket", t.TempDir())
	m.Composer.SetValue("hello world")
	updated, cmd := m.handleChatKey(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if cmd == nil || !got.Pending || got.Composer.Value() != "" {
		t.Fatalf("submit state not applied: pending=%v text=%q", got.Pending, got.Composer.Value())
	}
}
