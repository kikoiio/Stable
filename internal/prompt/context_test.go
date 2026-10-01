package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"stable/internal/decision"
	"stable/internal/sessionlog"
	"strings"
	"testing"
)

func TestStableSystemAndSessionProjection(t *testing.T) {
	if StableSystem() != StableSystem() {
		t.Fatal("stable prompt changed between builds")
	}
	r := sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: "a"}, Events: []sessionlog.Event{
		{Seq: 1, Type: sessionlog.EventSessionCreated},
		{Seq: 2, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "only A"}},
	}}
	msgs := Project(r, "")
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Content != "only A" {
		t.Fatalf("projection: %+v", msgs)
	}
	if got := ApproxTokens(msgs); got <= 0 {
		t.Fatalf("token estimate %d", got)
	}
}

type summaryStub struct{}

func (summaryStub) GenerateChat(context.Context, []decision.ChatMessage) (string, error) {
	return "summary", nil
}

type failingSummary struct{}

func (failingSummary) GenerateChat(context.Context, []decision.ChatMessage) (string, error) {
	return "", errors.New("provider failed")
}

func TestCompactPreservesOriginalAndProjectsSummaryTail(t *testing.T) {
	root := t.TempDir()
	s, err := sessionlog.Create(root, "compact")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err = sessionlog.Append(root, s.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: fmt.Sprintf("msg-%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := sessionlog.Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Compact(context.Background(), root, s.ID, before.Events, 3, summaryStub{}); err != nil {
		t.Fatal(err)
	}
	after, err := sessionlog.Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) <= len(before.Events) {
		t.Fatal("original events were rewritten or removed")
	}
	projection := Project(after, "")
	joined := ""
	for _, m := range projection {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "summary") || !strings.Contains(joined, "msg-11") || strings.Contains(joined, "msg-00") {
		t.Fatalf("wrong compacted projection: %s", joined)
	}
}

func TestSummaryFailureDoesNotMutateLog(t *testing.T) {
	root := t.TempDir()
	s, err := sessionlog.Create(root, "failure")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err = sessionlog.Append(root, s.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: fmt.Sprintf("old-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := sessionlog.Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := sessionlog.SessionPath(root, s.ID)
	original, _ := os.ReadFile(path)
	if _, err = Compact(context.Background(), root, s.ID, before.Events, 2, failingSummary{}); err == nil {
		t.Fatal("summary failure was hidden")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("failed compaction mutated the append-only log")
	}
}

func TestOversizedToolResultIsBudgetedButStoredWhole(t *testing.T) {
	root := t.TempDir()
	s, err := sessionlog.Create(root, "tool-result")
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("payload", 3000)
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventToolCall, sessionlog.ToolCall{CallID: "call-1", Name: "future-tool", Input: map[string]any{"path": "example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, s.ID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: "call-1", Result: large}); err != nil {
		t.Fatal(err)
	}
	replay, err := sessionlog.Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection := Project(replay, "")
	var resultMessage string
	for _, m := range projection {
		if strings.Contains(m.Content, "recorded tool result") {
			resultMessage = m.Content
		}
	}
	if len(resultMessage) == 0 || len([]rune(resultMessage)) > 8100 || !strings.Contains(resultMessage, "truncated for model context") {
		t.Fatalf("result projection not budgeted: %d chars", len([]rune(resultMessage)))
	}
	var stored sessionlog.ToolResult
	raw, _ := json.Marshal(replay.Events[2].Data)
	if json.Unmarshal(raw, &stored) != nil || stored.Result != large {
		t.Fatal("full tool result was lost from the event log")
	}
}

func TestFitRetainsSystemAndRecentTail(t *testing.T) {
	msgs := []decision.ChatMessage{{Role: "system", Content: "fixed system prompt"}, {Role: "user", Content: string(make([]byte, 1000))}, {Role: "user", Content: "recent"}}
	fit := Fit(msgs, 30)
	if len(fit) < 2 || fit[0].Role != "system" || fit[len(fit)-1].Content != "recent" {
		t.Fatalf("fit lost system or recent turn: %+v", fit)
	}
}

func TestToolResultViewIsBounded(t *testing.T) {
	result := BudgetToolResult(strings.Repeat("界", 1000), 80)
	if got := len([]rune(result)); got > 80 {
		t.Fatalf("tool result view has %d chars", got)
	}
	if !strings.Contains(result, "truncated for model context") {
		t.Fatalf("missing truncation marker: %q", result)
	}
}
