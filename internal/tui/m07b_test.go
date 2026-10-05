package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/sessionlog"
)

func TestHooksCommandSendsListAndReloadOps(t *testing.T) {
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	m.Composer.SetValue("/hooks")
	_, cmd := m.submitComposer()
	if cmd == nil {
		t.Fatal("/hooks should send hooks_list")
	}
	if msg, ok := cmd().(resultMsg); !ok || msg.op != "hooks_list" {
		t.Fatalf("expected hooks_list, got %T", cmd())
	}

	m2 := New("sock", t.TempDir())
	m2.ActiveSession = "s1"
	m2.Composer.SetValue("/hooks reload")
	_, cmd2 := m2.submitComposer()
	if msg, ok := cmd2().(resultMsg); !ok || msg.op != "hooks_reload" {
		t.Fatalf("expected hooks_reload, got %T", cmd2())
	}
}

func TestHookListReportAndTranscript(t *testing.T) {
	m := New("sock", t.TempDir())
	updated, _ := m.handleResult(resultMsg{op: "hooks_list", msgs: []conversation.ServerMsg{{
		Type: "hook_list",
		HookList: &conversation.HookListMsg{
			Hooks:      []conversation.HookSummary{{ID: "block", Event: "pre_tool_use", Action: "prompt", Source: "project", Reject: true}},
			Rejections: []string{"user.yaml: duplicate id a skipped"},
		},
	}}})
	got := updated.(Model)
	var last sessionlog.Message
	b, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	if json.Unmarshal(b, &last) != nil || !strings.Contains(last.Text, "block") || !strings.Contains(last.Text, "reject") || !strings.Contains(last.Text, "跳过") {
		t.Fatalf("hook list render = %q", last.Text)
	}

	updated, _ = got.handleResult(resultMsg{op: "hooks_reload", msgs: []conversation.ServerMsg{{
		Type:       "hook_report",
		HookReport: &conversation.HookReportMsg{Before: 1, After: 2},
	}}})
	got = updated.(Model)
	if !strings.Contains(got.Status, "Hooks 重载: 1 → 2") {
		t.Fatalf("reload status = %q", got.Status)
	}

	events := []sessionlog.Event{
		{Type: sessionlog.EventHookFired, Data: sessionlog.HookFired{HookID: "block", Event: "pre_tool_use", Action: "prompt", Success: false, Rejected: true, Output: "no-bash"}},
		{Type: sessionlog.EventHookReload, Data: sessionlog.HookReload{Before: 1, After: 2}},
	}
	out := projectTranscript(events, 80, false)
	for _, want := range []string{"Hook", "block(pre_tool_use)", "已拒绝", "no-bash", "Hooks 重载", "1 → 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in transcript:\n%s", want, out)
		}
	}
}
