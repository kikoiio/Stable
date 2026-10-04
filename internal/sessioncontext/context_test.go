package sessioncontext

import (
	"context"
	"errors"
	"strings"
	"testing"

	"stable/internal/decision"
	"stable/internal/prompt"
	"stable/internal/sessionlog"
)

// testWindow keeps the trigger and budget arithmetic readable: trigger at
// 3276 tokens, input budget 2867.
const testWindow = 4096

type fakeProvider struct {
	responses []string
	err       error
	requests  [][]decision.ChatMessage
}

func (f *fakeProvider) GenerateChat(_ context.Context, msgs []decision.ChatMessage) (string, error) {
	f.requests = append(f.requests, msgs)
	if f.err != nil {
		return "", f.err
	}
	if len(f.responses) > 0 {
		return f.responses[0], nil
	}
	return "摘要：较早内容", nil
}

func messageItem(seq uint64, role, text string) sessionlog.Item {
	m := sessionlog.Message{Role: role, Text: text}
	return sessionlog.Item{Seq: seq, Kind: sessionlog.ItemMessage, Message: &m, Matched: true}
}

func prefix() []decision.ChatMessage {
	return []decision.ChatMessage{{Role: "system", Content: "system"}}
}

func TestEffectiveWindow(t *testing.T) {
	cases := []struct {
		configured int
		window     int
		fellBack   bool
	}{
		{0, DefaultWindowTokens, false},
		{-100, DefaultWindowTokens, true},
		{MinWindowTokens - 1, DefaultWindowTokens, true},
		{MinWindowTokens, MinWindowTokens, false},
		{128000, 128000, false},
	}
	for _, tc := range cases {
		window, fellBack := EffectiveWindow(tc.configured)
		if window != tc.window || fellBack != tc.fellBack {
			t.Fatalf("EffectiveWindow(%d) = %d,%t want %d,%t", tc.configured, window, fellBack, tc.window, tc.fellBack)
		}
	}
}

func TestPrepareUnderTriggerLeavesMessagesAlone(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	projection := sessionlog.Projection{Items: []sessionlog.Item{
		messageItem(1, "user", "hello"),
		messageItem(2, "assistant", "hi"),
	}}
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Compacted || prepared.Boundary != nil {
		t.Fatalf("unexpected compaction: %+v", prepared)
	}
	if len(provider.requests) != 0 {
		t.Fatal("summarizer called below the trigger")
	}
	if len(prepared.Messages) != 3 {
		t.Fatalf("messages = %+v", prepared.Messages)
	}
}

func bigProjection(n int, size int) sessionlog.Projection {
	items := make([]sessionlog.Item, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, messageItem(uint64(i+1), "user", strings.Repeat("x", size)))
	}
	return sessionlog.Projection{Items: items}
}

func TestPrepareCompactsWithSessionBoundary(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	// 80 messages of 200 chars ≈ 4320 tokens > 3276 trigger.
	projection := bigProjection(80, 200)
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Compacted || prepared.Boundary == nil {
		t.Fatalf("expected compaction: %+v", prepared)
	}
	b := prepared.Boundary
	if b.EffectiveScope() != sessionlog.BoundaryScopeSession || b.RunID != "" {
		t.Fatalf("boundary scope = %+v", b)
	}
	if b.FromSeq == 0 || b.ToSeq < b.FromSeq || b.ToSeq >= 80 {
		t.Fatalf("boundary range = %+v", b)
	}
	if len(provider.requests) == 0 {
		t.Fatal("summarizer not called")
	}
	joined := ""
	for _, m := range prepared.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "摘要") {
		t.Fatalf("summary missing from request: %s", joined)
	}
	if got := prompt.ApproxTokens(prepared.Messages); got > manager.inputBudget() {
		t.Fatalf("request %d tokens over budget %d", got, manager.inputBudget())
	}
}

func TestPrepareKeepsRecentTailVerbatim(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	items := []sessionlog.Item{}
	for i := 0; i < 79; i++ {
		items = append(items, messageItem(uint64(i+1), "user", strings.Repeat("x", 200)))
	}
	items = append(items, messageItem(80, "user", "最近的原文标记"))
	projection := sessionlog.Projection{Items: items}
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Compacted {
		t.Fatal("expected compaction")
	}
	last := prepared.Messages[len(prepared.Messages)-1]
	if last.Content != "最近的原文标记" {
		t.Fatalf("recent tail not preserved verbatim: %q", last.Content)
	}
	if prepared.Boundary.ToSeq >= 80 {
		t.Fatalf("boundary covered the recent tail: %+v", prepared.Boundary)
	}
}

func TestPrepareProtectsToolPairs(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	items := []sessionlog.Item{}
	for i := 0; i < 78; i++ {
		items = append(items, messageItem(uint64(i+1), "user", strings.Repeat("x", 200)))
	}
	call := sessionlog.ToolCall{CallID: "c1", Name: "write"}
	result := sessionlog.ToolResult{CallID: "c1", Result: "ok"}
	items = append(items,
		sessionlog.Item{Seq: 79, Kind: sessionlog.ItemToolCall, Call: &call, Matched: true},
		sessionlog.Item{Seq: 80, Kind: sessionlog.ItemToolResult, Result: &result, Matched: true},
		messageItem(81, "user", "tail"),
	)
	projection := sessionlog.Projection{Items: items}
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Compacted {
		t.Fatal("expected compaction")
	}
	b := prepared.Boundary
	// The pair must be fully inside or fully outside the covered range.
	callCovered := b.ToSeq >= 79
	resultCovered := b.ToSeq >= 80
	if callCovered != resultCovered {
		t.Fatalf("boundary split the tool pair: %+v", b)
	}
}

func TestPrepareNeverCoversDanglingCall(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	items := []sessionlog.Item{}
	for i := 0; i < 80; i++ {
		items = append(items, messageItem(uint64(i+1), "user", strings.Repeat("x", 200)))
	}
	dangling := sessionlog.ToolCall{CallID: "c9", Name: "write"}
	items = append(items, sessionlog.Item{Seq: 81, Kind: sessionlog.ItemToolCall, Call: &dangling, Matched: false})
	projection := sessionlog.Projection{Items: items}
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Compacted && prepared.Boundary.ToSeq >= 81 {
		t.Fatalf("boundary covered an unmatched tool call: %+v", prepared.Boundary)
	}
}

func TestPrepareSummaryFailureReturnsError(t *testing.T) {
	provider := &fakeProvider{err: errors.New("provider down")}
	manager, _ := NewManager(testWindow, provider)
	if _, err := manager.Prepare(context.Background(), bigProjection(80, 200), prefix()); err == nil {
		t.Fatal("summary failure was hidden")
	}
}

func TestPrepareWithoutProviderFailsVisibly(t *testing.T) {
	manager, _ := NewManager(testWindow, nil)
	if _, err := manager.Prepare(context.Background(), bigProjection(80, 200), prefix()); err == nil {
		t.Fatal("over-budget request without summarizer was prepared")
	}
}

func TestPrepareRejectsUnfittableTail(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	// One giant message cannot be compacted away below itself.
	projection := sessionlog.Projection{Items: []sessionlog.Item{
		messageItem(1, "user", strings.Repeat("x", 40000)),
	}}
	if _, err := manager.Prepare(context.Background(), projection, prefix()); err == nil {
		t.Fatal("unfittable context was prepared")
	}
}

func TestCompactionInputIsBounded(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(128000, provider)
	// 120 messages of 4000 chars ≈ 120k tokens > 102k trigger; the covered
	// range then exceeds the 48k-char summarizer input cap.
	items := []sessionlog.Item{}
	for i := 0; i < 120; i++ {
		items = append(items, messageItem(uint64(i+1), "user", strings.Repeat("x", 4000)))
	}
	projection := sessionlog.Projection{Items: items}
	prepared, err := manager.Prepare(context.Background(), projection, prefix())
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Compacted {
		t.Fatal("expected compaction")
	}
	if prepared.Boundary.FromSeq == items[0].Seq {
		t.Fatal("uncapped compaction input")
	}
	for _, request := range provider.requests {
		for _, m := range request {
			if m.Role == "user" && len(m.Content) > summaryChunkChars+summaryMaxChars+200 {
				t.Fatalf("single summarizer request has %d chars", len(m.Content))
			}
		}
	}
}
