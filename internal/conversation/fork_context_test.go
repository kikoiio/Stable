package conversation

import (
	"context"
	"strings"
	"testing"

	"stable/internal/sessionlog"
)

func appendForkContextMessage(t *testing.T, root, sessionID, role, kind, text string) {
	t.Helper()
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventMessage, sessionlog.Message{Role: role, Kind: kind, Text: text}); err != nil {
		t.Fatal(err)
	}
}

func TestForkContextRecentUsesLastFiveVisibleRounds(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "fork context")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		appendForkContextMessage(t, root, session.ID, "user", "text", "question "+string(rune('0'+i)))
		appendForkContextMessage(t, root, session.ID, "assistant", "text", "answer "+string(rune('0'+i)))
	}
	// Internal message kinds must not count as visible rounds or leak into a fork.
	appendForkContextMessage(t, root, session.ID, "assistant", "thought", "private thought")

	got, err := NewForkContextSource(root, 8192).Build(context.Background(), session.ID, "parent", ForkContextRecent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d messages, want 5 visible rounds (10 messages): %#v", len(got), got)
	}
	if got[0].Content != "question 3" || got[len(got)-1].Content != "answer 7" {
		t.Fatalf("recent context did not select the last five rounds: %#v", got)
	}
	for _, message := range got {
		if strings.Contains(message.Content, "private thought") {
			t.Fatalf("thought leaked into fork context: %#v", got)
		}
	}
}

func TestForkContextFullIsBoundedAndSessionScoped(t *testing.T) {
	root := t.TempDir()
	first, err := sessionlog.Create(root, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Create(root, "second")
	if err != nil {
		t.Fatal(err)
	}
	appendForkContextMessage(t, root, first.ID, "user", "text", strings.Repeat("old ", 1000))
	appendForkContextMessage(t, root, first.ID, "user", "text", "latest question")
	appendForkContextMessage(t, root, first.ID, "assistant", "text", "keep this latest answer")
	appendForkContextMessage(t, root, second.ID, "user", "text", "other session secret")

	got, err := NewForkContextSource(root, 1024).Build(context.Background(), first.ID, "parent", ForkContextFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "keep this latest answer" {
		t.Fatalf("full context should trim oldest content to configured budget: %#v", got)
	}
	if forkApproxTokens(got) > forkInputBudget(1024) {
		t.Fatalf("context exceeds budget: tokens=%d budget=%d", forkApproxTokens(got), forkInputBudget(1024))
	}
}

func TestForkContextNoneAndUnknownMode(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "fork context")
	if err != nil {
		t.Fatal(err)
	}
	source := NewForkContextSource(root, 8192)
	got, err := source.Build(context.Background(), session.ID, "parent", ForkContextNone)
	if err != nil || len(got) != 0 {
		t.Fatalf("none mode returned %v, %v; want empty context", got, err)
	}
	if _, err := source.Build(context.Background(), session.ID, "parent", ForkContextMode("future")); err == nil {
		t.Fatal("unknown context mode unexpectedly succeeded")
	}
}
