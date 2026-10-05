package hooks

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateCondition(t *testing.T) {
	ctx := Context{Event: EventPreToolUse, ToolName: "read_file", FilePath: "/tmp/a.go", Message: "done", ToolArgs: map[string]any{"path": "/tmp/a.go"}}
	cases := map[string]bool{
		`tool == read_file`: true, `tool != write_file`: true, `file_path =~ \.go$`: true,
		`file_path =* /tmp/*.go`: true, `event == pre_tool_use && args.path == /tmp/a.go`: true,
		`tool == write_file || message == done`: true, `!(tool == write_file)`: true,
		`tool == write_file && message == done`: false,
	}
	for cond, want := range cases {
		if got := EvaluateCondition(cond, ctx); got != want {
			t.Errorf("%q: got %v, want %v", cond, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	err := Validate([]Hook{{ID: "bad", Event: "nope", Action: Action{Type: "command"}, OnError: "wat"}, {Action: Action{Type: "prompt"}}})
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "hook[1]") {
		t.Fatalf("expected aggregated labeled errors, got %v", err)
	}
}

func TestFireCommandAndOrder(t *testing.T) {
	list := []Hook{
		{ID: "one", Event: EventRunStart, Action: Action{Type: "command", Command: `printf '%s/%s/%s' "$STABLE_EVENT" "$STABLE_TOOL" "$STABLE_FILE_PATH"`}},
		{ID: "two", Event: EventRunStart, Action: Action{Type: "prompt", Message: "notice"}},
	}
	got := Fire(list, Context{Event: EventRunStart, ToolName: "tool", FilePath: "file"})
	if len(got) != 2 || got[0].HookID != "one" || got[1].HookID != "two" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[0].Output != "run_start/tool/file" || !got[0].Success || !got[1].Success {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestFireRejectAndDisabledActions(t *testing.T) {
	got := FireOne(Hook{ID: "reject", Reject: true, Action: Action{Type: "prompt", Message: "blocked"}}, Context{})
	if !got.Success || !got.Rejected || got.Output != "blocked" {
		t.Fatalf("unexpected rejection: %+v", got)
	}
	for _, typ := range []string{"http", "agent"} {
		r := FireOne(Hook{ID: typ, Action: Action{Type: typ}}, Context{})
		if r.Success || !strings.Contains(r.Output, "not enabled") || !r.Rejected == true { /* rejection is only on reject policy */
		}
		if r.Success || !strings.Contains(r.Output, "not enabled") {
			t.Fatalf("unexpected %s result: %+v", typ, r)
		}
	}
}

func TestFireTimeout(t *testing.T) {
	r := FireOne(Hook{Action: Action{Type: "command", Command: "sleep 1", Timeout: 10 * time.Millisecond}}, Context{})
	if r.Success || !r.TimedOut {
		t.Fatalf("expected timeout: %+v", r)
	}
}
