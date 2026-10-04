package prompt

import (
	"encoding/json"
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

func TestMessagesFromItemsDropsDanglingToolCall(t *testing.T) {
	items := []sessionlog.Item{
		{Seq: 1, Kind: sessionlog.ItemMessage, Message: &sessionlog.Message{Role: "user", Text: "go"}, Matched: true},
		{Seq: 2, Kind: sessionlog.ItemToolCall, Call: &sessionlog.ToolCall{CallID: "c1", Name: "write"}, Matched: false},
	}
	msgs := MessagesFromItems(items)
	if len(msgs) != 1 || msgs[0].Content != "go" {
		t.Fatalf("dangling call leaked into recoverable tail: %+v", msgs)
	}
	// A matched call and its result both survive.
	items[1].Matched = true
	items = append(items, sessionlog.Item{Seq: 3, Kind: sessionlog.ItemToolResult, Result: &sessionlog.ToolResult{CallID: "c1", Result: "ok"}, Matched: true})
	msgs = MessagesFromItems(items)
	if len(msgs) != 3 {
		t.Fatalf("complete pair not preserved: %+v", msgs)
	}
}

func TestProjectSubstitutesBoundarySummary(t *testing.T) {
	r := sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: "a"}, Events: []sessionlog.Event{
		{Seq: 1, Type: sessionlog.EventSessionCreated},
		{Seq: 2, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "msg-00"}},
		{Seq: 3, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "assistant", Text: "msg-01"}},
		{Seq: 4, Type: sessionlog.EventBoundary, Data: sessionlog.Boundary{FromSeq: 2, ToSeq: 3, Summary: "summary"}},
		{Seq: 5, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "msg-11"}},
	}}
	msgs := Project(r, "")
	joined := ""
	for _, m := range msgs {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "summary") || !strings.Contains(joined, "msg-11") || strings.Contains(joined, "msg-00") {
		t.Fatalf("wrong compacted projection: %s", joined)
	}
}

func TestOversizedToolResultIsBudgetedButStoredWhole(t *testing.T) {
	r := sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: "a"}, Events: []sessionlog.Event{
		{Seq: 1, Type: sessionlog.EventSessionCreated},
		{Seq: 2, Type: sessionlog.EventToolCall, Data: sessionlog.ToolCall{CallID: "call-1", Name: "future-tool", Input: map[string]any{"path": "example"}}},
		{Seq: 3, Type: sessionlog.EventToolResult, Data: sessionlog.ToolResult{CallID: "call-1", Result: strings.Repeat("payload", 3000)}},
	}}
	projection := Project(r, "")
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
	raw, _ := json.Marshal(r.Events[2].Data)
	if json.Unmarshal(raw, &stored) != nil || stored.Result != strings.Repeat("payload", 3000) {
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
