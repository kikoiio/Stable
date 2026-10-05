package sessionlog

import (
	"strings"
	"testing"
)

func TestHookEventsRoundTripAndProjection(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventHookFired, HookFired{HookID: "a", Event: "run_start", Action: "prompt", Success: true, Output: "hi"}); err != nil {
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
	if p.Items[0].HookFired == nil || p.Items[0].HookFired.HookID != "a" {
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
