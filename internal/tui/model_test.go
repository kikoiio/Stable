package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"testing"
)

func TestSessionNavigationAndResizeStayAvailable(t *testing.T) {
	m := New("socket", t.TempDir())
	m.Sessions = []sessionlog.SessionInfo{{ID: "one", Title: "one"}, {ID: "two", Title: "two"}}
	m.ActiveSession = "one"
	m.Pending = true
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	got := updated.(Model)
	if got.ActiveSession != "two" {
		t.Fatalf("down did not navigate during pending request: %s", got.ActiveSession)
	}
	updated, _ = got.Update(tea.WindowSizeMsg{Width: 50, Height: 15})
	got = updated.(Model)
	if got.Width != 50 || got.Height != 15 {
		t.Fatalf("resize not applied: %dx%d", got.Width, got.Height)
	}
	if got.View() == "" {
		t.Fatal("view is empty")
	}
}

func TestSessionListSelectsMostRecentAndLoadsIt(t *testing.T) {
	m := New("socket", t.TempDir())
	updated, cmd := m.handleResult(resultMsg{op: "session_list", msgs: []conversation.ServerMsg{{Type: "sessions", Sessions: []sessionlog.SessionInfo{{ID: "recent"}, {ID: "old"}}}}})
	got := updated.(Model)
	if got.ActiveSession != "recent" || cmd == nil {
		t.Fatalf("recent session not selected/loaded: %+v cmd=%v", got, cmd)
	}
}
