package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/memory"
	"stable/internal/sessionlog"
)

func TestMemoryCommandsDispatchScopedRequests(t *testing.T) {
	tests := []struct {
		input string
		op    string
		scope string
		entry string
	}{
		{"/memory list", "memory_list", "", ""},
		{"/memory delete project overview.md", "memory_delete", "project", "overview.md"},
		{"/memory clear", "memory_clear", "", ""},
		{"/memory clear user", "memory_clear", "user", ""},
		{"/memory clear all", "memory_clear", "all", ""},
	}
	for _, test := range tests {
		m := New("sock", t.TempDir())
		m.ActiveSession = "session-1"
		m.Composer.SetValue(test.input)
		_, cmd := m.submitComposer()
		if cmd == nil {
			t.Fatalf("%s did not dispatch", test.input)
		}
		result, ok := cmd().(resultMsg)
		if !ok || result.op != test.op {
			t.Fatalf("%s result = %#v", test.input, result)
		}
	}
}

func TestMemoryCommandUsageAndDefaultClear(t *testing.T) {
	m := New("sock", t.TempDir())
	m.ActiveSession = "session-1"
	m.Composer.SetValue("/memory")
	updated, cmd := m.submitComposer()
	if cmd != nil {
		t.Fatal("/memory without arguments should show usage locally")
	}
	got := updated.(Model)
	var note sessionlog.Message
	data, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	_ = json.Unmarshal(data, &note)
	if !strings.Contains(note.Text, "/memory clear [user|all]") {
		t.Fatalf("usage = %q", note.Text)
	}

	clear := New("sock", t.TempDir())
	clear.ActiveSession = "session-1"
	clear.Composer.SetValue("/memory clear")
	_, cmd = clear.submitComposer()
	request, ok := cmd().(resultMsg)
	if !ok || request.op != "memory_clear" {
		t.Fatalf("default clear op = %#v", request)
	}
}

func TestMemoryResponsesRenderHeadersAndErrors(t *testing.T) {
	m := New("sock", t.TempDir())
	updated, _ := m.handleResult(resultMsg{op: "memory_list", msgs: []conversation.ServerMsg{{
		Type: "memory_list", MemoryEntries: []memory.MemoryHeader{{Scope: memory.ScopeProject, Type: memory.TypeReference, Filename: "overview.md", Name: "Overview", Description: "Architecture notes"}},
	}}})
	got := updated.(Model)
	var note sessionlog.Message
	data, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	if json.Unmarshal(data, &note) != nil || !strings.Contains(note.Text, "project") || !strings.Contains(note.Text, "reference") || !strings.Contains(note.Text, "Overview") || !strings.Contains(note.Text, "overview.md") {
		t.Fatalf("memory list render = %q", note.Text)
	}
	updated, _ = got.handleResult(resultMsg{op: "memory_clear", msgs: []conversation.ServerMsg{{
		Type: "memory_report", MemoryReport: &conversation.MemoryReportMsg{Operation: "clear", Scope: "project", Deleted: 2, Error: "unsafe entry remains"},
	}}})
	got = updated.(Model)
	if !strings.Contains(got.Status, "unsafe entry remains") {
		t.Fatalf("memory error status = %q", got.Status)
	}
}

func TestMemoryBackgroundStatusAppearsInTranscript(t *testing.T) {
	text := projectTranscript([]sessionlog.Event{
		{Type: sessionlog.EventMemoryBackground, Data: sessionlog.MemoryBackgroundRecord{Action: "extract", State: "failed", Reason: "provider unavailable"}},
		{Type: sessionlog.EventMemoryAction, Data: sessionlog.MemoryActionRecord{Scope: "user", Entry: "tone.md", Operation: "save", State: "success"}},
	}, 80, false)
	for _, want := range []string{"记忆后台", "extract failed", "provider unavailable", "记忆操作", "user/save tone.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript missing %q:\n%s", want, text)
		}
	}
	m := New("sock", t.TempDir())
	m.applyRunMessage(conversation.ServerMsg{Type: "memory_background", MemoryBackground: &sessionlog.MemoryBackgroundRecord{Action: "extract", State: "failed", Reason: "provider unavailable"}})
	if !strings.Contains(m.Status, "provider unavailable") || !strings.Contains(m.Status, "failed") {
		t.Fatalf("live memory status = %q", m.Status)
	}
}
