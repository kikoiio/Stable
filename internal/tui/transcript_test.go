package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/sessionlog"
	"strings"
	"testing"
)

func TestRenderMarkdownStructureAndNoColor(t *testing.T) {
	input := "# Heading\n\n- one\n- two\n\n```go\nfmt.Println(\"ok\")\n```\n\nA long paragraph stays complete."
	out := renderMarkdown(input, 36, false)
	for _, part := range []string{"Heading", "one", "two", "fmt.Println", "A long paragraph stays complete"} {
		if !strings.Contains(out, part) {
			t.Fatalf("missing %q in %s", part, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("no-color output contains ANSI: %q", out)
	}
}

func TestTranscriptProjectionAndSessionReset(t *testing.T) {
	events := []sessionlog.Event{{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "first\nsecond"}}, {Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "assistant", Text: "reply"}}}
	out := projectTranscript(events, 60, false)
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") || !strings.Contains(out, "reply") {
		t.Fatalf("lost message text: %s", out)
	}
	v := Transcript{}
	v.SetSize(24, 4)
	v.SetEvents(events)
	v.SetEvents([]sessionlog.Event{{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "new session"}}})
	if strings.Contains(v.View(), "first") {
		t.Fatal("old session remained after replacing events")
	}
}

func TestTranscriptRendersM06Events(t *testing.T) {
	events := []sessionlog.Event{
		{Type: sessionlog.EventPlanMode, Data: sessionlog.PlanMode{Mode: sessionlog.PlanModePlan, Reason: sessionlog.PlanModeReasonUserToggle}},
		{Type: sessionlog.EventPlanApproval, Data: sessionlog.PlanApprovalRecord{RequestID: "plan-1", Status: sessionlog.PlanApprovalSubmitted, PlanPath: "/p/.stable/plans/s1.md"}},
		{Type: sessionlog.EventPlanApproval, Data: sessionlog.PlanApprovalRecord{RequestID: "plan-1", Status: sessionlog.PlanApprovalFeedback, PlanPath: "/p/.stable/plans/s1.md", Feedback: "补充验证章节"}},
		{Type: sessionlog.EventTodo, Data: sessionlog.TodoUpdate{Revision: 1, Tasks: []sessionlog.TaskSnapshot{{ID: "task-1", Subject: "调研现状", Status: "in_progress"}, {ID: "task-2", Subject: "写计划", Status: "pending"}}}},
	}
	out := projectTranscript(events, 80, false)
	for _, want := range []string{"进入计划模式（用户切换）", "待审批：/p/.stable/plans/s1.md", "反馈继续改", "补充验证章节", "[in_progress] 调研现状", "[pending] 写计划"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in transcript:\n%s", want, out)
		}
	}
}

func TestTranscriptViewportScrollAndResize(t *testing.T) {
	events := make([]sessionlog.Event, 0, 20)
	for i := 0; i < 20; i++ {
		events = append(events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: strings.Repeat("line ", 5) + string(rune('A'+i))}})
	}
	v := Transcript{}
	v.SetSize(32, 4)
	v.SetEvents(events)
	if !v.Viewport.AtBottom() {
		t.Fatal("transcript did not open at latest message")
	}
	v.Viewport.LineUp(3)
	if v.Viewport.AtBottom() {
		t.Fatal("viewport did not scroll")
	}
	v.SetSize(40, 6)
	if v.Width != 40 || v.Height != 6 {
		t.Fatalf("resize not applied: %dx%d", v.Width, v.Height)
	}
}

func TestTranscriptKeyboardScrollSurvivesModelResize(t *testing.T) {
	m := New("", t.TempDir())
	m.Transcript.SetSize(32, 4)
	var events []sessionlog.Event
	for i := 0; i < 30; i++ {
		events = append(events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "assistant", Text: strings.Repeat("long response line ", 8) + string(rune('A'+i%26))}})
	}
	m.Events = events
	m.Transcript.SetEvents(events)
	if !m.Transcript.Viewport.AtBottom() {
		t.Fatal("transcript did not start at bottom")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(Model)
	if m.Transcript.Viewport.AtBottom() {
		t.Fatal("PageUp did not scroll transcript")
	}
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 48, Height: 18})
	m = updated.(Model)
	if m.Transcript.Viewport.AtBottom() {
		t.Fatalf("resize reset scroll position to bottom (offset=%d height=%d percent=%.2f)", m.Transcript.Viewport.YOffset, m.Transcript.Viewport.Height, m.Transcript.Viewport.ScrollPercent())
	}
}
