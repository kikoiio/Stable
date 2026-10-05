package sessionlog_test

import (
	"testing"

	"stable/internal/prompt"
	"stable/internal/sessionlog"
)

// TestSkillItemsStayOutOfModelContext pins the exclusion contract for the
// M07 skill events: they are projected for the transcript and audit, but
// prompt.MessagesFromItems (the session message rebuild) must not consume
// them, so skill events never enter the model context. This mirrors how the
// M06 plan/todo items are excluded by the same switch.
func TestSkillItemsStayOutOfModelContext(t *testing.T) {
	root := t.TempDir()
	s, err := sessionlog.Create(root, "skill-context")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventSkillInventory, sessionlog.SkillInventory{Skills: []sessionlog.SkillInfo{
		{Name: "code-review", Description: "Review the diff", WhenToUse: "before commits", Source: "user"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventSkillDelta, sessionlog.SkillDelta{Added: []sessionlog.SkillInfo{
		{Name: "fresh-skill", Description: "New", Source: "project"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventSkillInvoked, sessionlog.SkillInvoked{Name: "code-review", Source: "user", Entry: sessionlog.SkillEntrySlash, Args: "focus"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	replay, err := sessionlog.Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection := sessionlog.Project(replay)
	// Sanity check: the three skill items are projected for the transcript.
	skillItems := 0
	for _, item := range projection.Items {
		switch item.Kind {
		case sessionlog.ItemSkillInventory, sessionlog.ItemSkillDelta, sessionlog.ItemSkillInvoked:
			skillItems++
		}
	}
	if skillItems != 3 {
		t.Fatalf("skill items projected = %d, want 3", skillItems)
	}
	msgs := prompt.MessagesFromItems(projection.Items)
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Fatalf("skill items leaked into model context: %+v", msgs)
	}
}
