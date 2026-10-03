package tui

import (
	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"strings"
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

func TestRunTranscriptSeparatesThinkingAndShowsUnavailableUsage(t *testing.T) {
	events := []sessionlog.Event{
		{Seq: 1, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "question"}},
		{Seq: 2, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "thinking_delta", Payload: json.RawMessage(`{"text":"private reasoning"}`)}},
		{Seq: 3, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "text_delta", Payload: json.RawMessage(`{"text":"answer"}`)}},
		{Seq: 4, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "usage", Payload: json.RawMessage(`{"kind":"usage","usage":{"input_tokens":2}}`)}},
		{Seq: 5, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "future_event", Payload: json.RawMessage(`{}`)}},
		{Seq: 6, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "tool_call_start", Payload: json.RawMessage(`{"tool":{"id":"call-1","name":"read"}}`)}},
		{Seq: 7, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "tool_call_delta", Payload: json.RawMessage(`{"text":"{}"}`)}},
		{Seq: 8, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "tool_call_complete", Payload: json.RawMessage(`{"tool":{"id":"call-1","name":"read","complete":true}}`)}},
		{Seq: 9, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "r1", Kind: "terminal", Payload: json.RawMessage(`{"status":"awaiting_tools"}`)}},
	}
	view := projectTranscript(events, 80, false)
	for _, want := range []string{"user", "思考（仅供查看）", "private reasoning", "Stable", "answer", "input=2", "output=unavailable", "cache_read=unavailable", "工具调用", "call-1", "运行状态", "awaiting tools"} {
		if !strings.Contains(view, want) {
			t.Fatalf("transcript missing %q: %s", want, view)
		}
	}
}

func TestTUIAppendsOnlyActiveRunEvents(t *testing.T) {
	m := New("", t.TempDir())
	m.ActiveRunID = "active"
	m.Pending = true
	m.applyRunMessage(conversation.ServerMsg{Type: "run_event", Cursor: 1, RunEvent: &sessionlog.RunEvent{ID: "other", RunID: "other", SessionID: "s", Kind: "text_delta"}})
	if len(m.Events) != 0 {
		t.Fatalf("foreign run leaked into transcript: %+v", m.Events)
	}
	m.applyRunMessage(conversation.ServerMsg{Type: "run_event", Cursor: 2, RunEvent: &sessionlog.RunEvent{ID: "active-1", RunID: "active", SessionID: "s", Kind: "text_delta"}})
	if len(m.Events) != 1 || m.Events[0].Seq != 2 {
		t.Fatalf("active run event missing: %+v", m.Events)
	}
}

func TestChatPrimaryViewAndDraftSurvivesNavigation(t *testing.T) {
	m := New("socket", t.TempDir())
	m.Width, m.Height = 100, 28
	m.Sessions = []sessionlog.SessionInfo{{ID: "one", Title: "one"}}
	m.ActiveSession = "one"
	m.Composer.SetValue("unfinished draft")
	view := m.View()
	if !strings.Contains(view, "输入消息") && !strings.Contains(view, "unfinished draft") {
		t.Fatalf("chat composer missing: %s", view)
	}
	if strings.Contains(view, "GLOBAL GOALS") || strings.Contains(view, "SESSIONS") {
		t.Fatalf("old permanent panels remain: %s", view)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if m.Navigation.Mode != SessionPickerView {
		t.Fatal("session navigation did not open")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.Composer.Value() != "unfinished draft" {
		t.Fatalf("navigation lost draft: %q", m.Composer.Value())
	}
}

func TestGoalNavigationShowsStatusAndEvidence(t *testing.T) {
	m := New("", t.TempDir())
	m.Navigation.Mode = GoalPickerView
	m.Goals = []core.Goal{{ID: "g1", Objective: "ship feature", Status: core.GoalWaiting, Reason: "needs review", EvidenceSummary: "tests passed"}}
	out := m.View()
	for _, want := range []string{"ship feature", "waiting", "needs review", "tests passed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q from goal view: %s", want, out)
		}
	}
}

func TestSlashCommandsOnlySwitchLocalNavigation(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want ViewMode
	}{{"/sessions", SessionPickerView}, {"/goals", GoalPickerView}} {
		m := New("socket", t.TempDir())
		m.Composer.SetValue(tc.cmd)
		updated, cmd := m.submitComposer()
		got := updated.(Model)
		if cmd != nil || got.Navigation.Mode != tc.want || got.Pending {
			t.Fatalf("%s sent a request or failed navigation: mode=%v pending=%v", tc.cmd, got.Navigation.Mode, got.Pending)
		}
	}
}

func TestModelResizeAndServiceErrorKeepDraftAndExposeError(t *testing.T) {
	m := New("socket", t.TempDir())
	m.Composer.SetValue("draft remains")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 18})
	m = updated.(Model)
	if m.Composer.Value() != "draft remains" {
		t.Fatal("resize lost composer draft")
	}
	updated, cmd := m.handleResult(resultMsg{op: "chat", err: errors.New("offline")})
	m = updated.(Model)
	if m.Err == nil || m.statusState().Phase != StatusError || (m.ActiveSession != "" && cmd == nil) {
		t.Fatalf("service error not represented: %+v", m)
	}
}

func TestSessionPickerUsesExistingSessionLoadOperation(t *testing.T) {
	m := New("/no/socket", t.TempDir())
	m.Sessions = []sessionlog.SessionInfo{{ID: "session-1"}}
	m.Navigation = NavigationState{Mode: SessionPickerView, Cursor: 0}
	_, cmd := m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("selecting session did not request load")
	}
	msg := cmd().(resultMsg)
	if msg.op != "session_load" {
		t.Fatalf("socket op changed: %s", msg.op)
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
