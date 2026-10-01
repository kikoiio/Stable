package prototype

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/prototype/demo"
)

func key(m Model, value string) Model {
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)})
	return updated.(Model)
}

func special(m Model, keyType tea.KeyType) Model {
	updated, _ := m.Update(tea.KeyMsg{Type: keyType})
	return updated.(Model)
}

func typeText(m Model, value string) Model {
	for _, r := range value {
		m = key(m, string(r))
	}
	return m
}

func TestKeyboardNavigatesSessionAndGlobalGoalPanels(t *testing.T) {
	m := NewModel()
	originalSessions := len(m.State.Sessions)
	m = key(m, "s")
	if len(m.State.Sessions) != originalSessions+1 || m.State.ActiveSessionID != m.State.Sessions[originalSessions].ID {
		t.Fatal("session creation shortcut did not create and select a demo session")
	}
	m = key(m, "1")
	m = special(m, tea.KeyUp)
	if m.State.ActiveSessionID != "session-notes" {
		t.Fatalf("session cursor did not move: %s", m.State.ActiveSessionID)
	}
	goalCount := len(m.State.Goals)
	m = key(m, "3")
	m = special(m, tea.KeyDown)
	if m.State.SelectedGoalID != m.State.Goals[1].ID || len(m.State.Goals) != goalCount {
		t.Fatal("global goal selection failed or changed the goal list")
	}
}

func TestGoalProposalConfirmRejectAndSteeringKeyboardFlow(t *testing.T) {
	m := NewModel()
	m = key(m, "n")
	m = typeText(m, "新增演示目标")
	m = special(m, tea.KeyEnter)
	if m.State.Sessions[0].Pending == nil || m.State.Sessions[0].Pending.Description != "新增演示目标" {
		t.Fatal("goal input did not create a proposal")
	}
	m = key(m, "y")
	if len(m.State.Goals) != 3 || m.State.Goals[2].SourceSessionID != m.State.ActiveSessionID {
		t.Fatal("proposal confirmation did not create an attributed global goal")
	}
	m = key(m, "r")
	m = typeText(m, "检查接口边界")
	m = special(m, tea.KeyEnter)
	if len(m.State.Goals[2].Steering) != 1 || !m.State.Goals[2].Steering[0].Demo {
		t.Fatal("steering did not attach to the selected goal")
	}
	m = key(m, "1")
	m = special(m, tea.KeyDown)
	if m.State.ActiveSessionID != "session-notes" {
		t.Fatal("session selection failed")
	}
	if m.State.Sessions[1].Pending != nil {
		t.Fatal("proposal leaked to another session")
	}
	before := len(m.State.Goals)
	m = key(m, "n")
	m = typeText(m, "拒绝演示目标")
	m = special(m, tea.KeyEnter)
	m = key(m, "x")
	if len(m.State.Goals) != before || m.State.Sessions[1].Pending != nil {
		t.Fatal("reject flow created a goal or left a pending proposal")
	}
}

func TestViewShowsDemoStatusAndResponsiveLayout(t *testing.T) {
	m := NewModel()
	view := m.View()
	if !strings.Contains(view, "DEMO") || !strings.Contains(view, "prop-demo-1") || !strings.Contains(view, "GLOBAL GOALS") {
		t.Fatalf("view is missing demo fixture content: %s", view)
	}
	m.Width = 100
	if !strings.Contains(m.View(), "2 CHAT / PROPOSAL") {
		t.Fatal("narrow layout did not show selected single panel")
	}
	beforeSession, beforeGoal := m.State.ActiveSessionID, m.State.SelectedGoalID
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	m = updated.(Model)
	if m.Width != 90 || m.State.ActiveSessionID != beforeSession || m.State.SelectedGoalID != beforeGoal {
		t.Fatal("resize failed to preserve selections")
	}
	m.Width = 120
	if !strings.Contains(m.View(), "1 SESSIONS") || !strings.Contains(m.View(), "3 GLOBAL GOALS") {
		t.Fatal("wide layout did not render three panels")
	}
}

func TestSmallTerminalShowsResizeHint(t *testing.T) {
	m := NewModel()
	m.Width, m.Height = 30, 8
	if got := m.View(); !strings.Contains(got, "扩大窗口") {
		t.Fatalf("small terminal hint missing: %q", got)
	}
}

func TestQuitKeyAndSelectPanel(t *testing.T) {
	m := NewModel()
	m = key(m, "2")
	if m.State.ActivePanel != demo.PanelChat {
		t.Fatal("chat panel shortcut failed")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if _, ok := updated.(Model); !ok || cmd == nil {
		t.Fatal("q did not request program exit")
	}
}
