package sessionlog

import (
	"reflect"
	"strings"
	"testing"
)

func TestHookEventsRoundTripAndProjection(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookFired, HookFired{HookID: "a", Event: "run_start", Action: "agent", Success: true, Output: "hi", RunID: "parent-run", ChildRunID: "child-run"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookFired, HookFired{HookID: "b", Event: "run_end", Action: "prompt", Success: true, Output: "bye"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookReload, HookReload{Before: 1, After: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookFired, HookFired{Event: "run_start", Action: "prompt"}); err == nil {
		t.Fatal("missing hook_id accepted")
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemHookFired, ItemHookFired, ItemHookReload}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("kinds=%v", got)
	}
	if p.Items[0].HookFired == nil || p.Items[0].HookFired.HookID != "a" || p.Items[0].HookFired.ChildRunID != "child-run" {
		t.Fatalf("projection missing hook: %+v", p.Items[0])
	}
}

func TestHookFiredOutputLimit(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "hooks-limit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookFired, HookFired{HookID: "a", Event: "run_start", Action: "prompt", Output: strings.Repeat("x", MaxHookOutput+1)}); err == nil {
		t.Fatal("oversize output accepted")
	}
}

func TestHookFiredChildRunIDLimit(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "hooks-child-id-limit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, s.ID, EventHookFired, HookFired{
		HookID: "agent", Event: "run_start", Action: "agent", ChildRunID: strings.Repeat("x", 129),
	}); err == nil {
		t.Fatal("expected oversized child run ID to be rejected")
	}
}

func TestMCPEventsRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	wantReload := MCPReload{Before: 1, After: 2, Rejections: []string{"legacy-fs"}, Trigger: "manual"}
	if _, err = Append(root, s.ID, EventMCPReload, wantReload); err != nil {
		t.Fatal(err)
	}
	wantConnected := MCPServer{Name: "docs", Source: "project", State: "connected", ToolCount: 3}
	if _, err = Append(root, s.ID, EventMCPServer, wantConnected); err != nil {
		t.Fatal(err)
	}
	wantFailed := MCPServer{Name: "docs", Source: "project", State: "reload-failed", Error: "handshake timeout"}
	if _, err = Append(root, s.ID, EventMCPServer, wantFailed); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Events) != 4 {
		t.Fatalf("events=%d", len(replay.Events))
	}
	var reload MCPReload
	if replay.Events[1].Type != EventMCPReload || decodeData(replay.Events[1].Data, &reload) != nil || !reflect.DeepEqual(reload, wantReload) {
		t.Fatalf("reload event=%+v data=%+v", replay.Events[1], reload)
	}
	var connected MCPServer
	if replay.Events[2].Type != EventMCPServer || decodeData(replay.Events[2].Data, &connected) != nil || !reflect.DeepEqual(connected, wantConnected) {
		t.Fatalf("server event=%+v data=%+v", replay.Events[2], connected)
	}
	var failed MCPServer
	if replay.Events[3].Type != EventMCPServer || decodeData(replay.Events[3].Data, &failed) != nil || !reflect.DeepEqual(failed, wantFailed) {
		t.Fatalf("server event=%+v data=%+v", replay.Events[3], failed)
	}
}

func TestMCPServerErrorLimit(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "mcp-limit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMCPServer, MCPServer{Name: "docs", Source: "project", State: "reload-failed", Error: strings.Repeat("x", MaxMCPOutput+1)}); err == nil {
		t.Fatal("oversize error accepted")
	}
}
