package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/hooks"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func writeHooksYAML(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func newHookFixture(t *testing.T, userBody, projectBody string) (*Service, *HookGate, string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	userPath := filepath.Join(t.TempDir(), "hooks.yaml")
	projectPath := filepath.Join(t.TempDir(), "hooks.yaml")
	if userBody != "" {
		writeHooksYAML(t, userPath, userBody)
	}
	if projectBody != "" {
		writeHooksYAML(t, projectPath, projectBody)
	}
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gate := NewHookGate(nil, userPath, projectPath)
	svc := &Service{deps: Deps{Store: db, ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{}}
	gate.Bind(svc)
	t.Cleanup(gate.Close)
	return svc, gate, root, session.ID, userPath, projectPath
}

func TestHookGateOnceAndOrder(t *testing.T) {
	_, gate, root, sessionID, _, _ := newHookFixture(t, `
hooks:
  - id: first
    event: run_start
    action: {type: prompt, message: one}
  - id: once
    event: run_start
    once: true
    action: {type: prompt, message: once-only}
  - id: skip
    event: run_start
    if: "tool == write_file"
    once: true
    action: {type: prompt, message: skipped}
`, "")
	gate.RunStart(sessionID, "run-1", "hello")
	gate.RunStart(sessionID, "run-2", "hello")
	text := gate.DrainNotifications(sessionID)
	if !strings.Contains(text, "one") || !strings.Contains(text, "once-only") || strings.Contains(text, "skipped") {
		t.Fatalf("unexpected notices: %s", text)
	}
	if strings.Count(text, "once-only") != 1 {
		t.Fatalf("once hook repeated: %s", text)
	}
	if second := gate.DrainNotifications(sessionID); second != "" {
		t.Fatalf("drain not idempotent: %s", second)
	}
	events := 0
	replay, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range replay.Events {
		if e.Type == sessionlog.EventHookFired {
			events++
		}
	}
	if events != 3 { // first+once on run1, first on run2
		t.Fatalf("hook_fired count=%d", events)
	}
}

func TestHookGatePreRejectAndQueueLimit(t *testing.T) {
	_, gate, _, sessionID, _, _ := newHookFixture(t, `
hooks:
  - id: block
    event: pre_tool_use
    reject: true
    action: {type: prompt, message: no-bash}
    if: "tool == command"
`, "")
	rejected, id, msg := gate.PreToolUse(sessionID, "command", map[string]any{"command": "rm"})
	if !rejected || id != "block" || !strings.Contains(msg, "no-bash") {
		t.Fatalf("reject=%v id=%s msg=%s", rejected, id, msg)
	}
	ok, _, _ := gate.PreToolUse(sessionID, "read_file", map[string]any{"path": "a.go"})
	if ok {
		t.Fatal("read_file should not be rejected")
	}
	for i := 0; i < 22; i++ {
		gate.enqueue(sessionID, "n", "x")
	}
	gate.mu.Lock()
	n := len(gate.queue[sessionID])
	gate.mu.Unlock()
	if n != maxHookNotifications {
		t.Fatalf("queue len=%d", n)
	}
	drained := gate.DrainNotifications(sessionID)
	if !strings.Contains(drained, "更早通知已丢弃") {
		t.Fatalf("missing overflow mark: %s", drained)
	}
}

func TestHookGateAsyncAndReload(t *testing.T) {
	_, gate, _, sessionID, userPath, _ := newHookFixture(t, `
hooks:
  - id: slow
    event: run_end
    async: true
    action: {type: command, command: "sleep 0.05; echo async-done", timeout: 2s}
`, "")
	start := time.Now()
	gate.RunEnd(sessionID, "run-1", "completed", "ok")
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("async hook blocked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(gate.DrainNotifications(sessionID), "async-done") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	writeHooksYAML(t, userPath, `
hooks:
  - id: extra
    event: run_start
    action: {type: prompt, message: new}
`)
	// bump mtime past filesystem resolution
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(userPath, future, future)
	before, after := gate.Reload()
	if after != 1 {
		t.Fatalf("reload before=%d after=%d", before, after)
	}
	summaries, _ := gate.List()
	if len(summaries) != 1 || summaries[0].ID != "extra" {
		t.Fatalf("list after reload: %+v", summaries)
	}
}

func TestHookGateRestartDropsQueue(t *testing.T) {
	_, gate, _, sessionID, userPath, projectPath := newHookFixture(t, `
hooks:
  - id: n
    event: run_start
    action: {type: prompt, message: queued}
`, "")
	gate.RunStart(sessionID, "r", "")
	fresh := NewHookGate(nil, userPath, projectPath)
	if text := fresh.DrainNotifications(sessionID); text != "" {
		t.Fatalf("queue survived restart: %s", text)
	}
	_ = hooks.EventRunStart
}
