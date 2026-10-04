package sessioncontext

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"stable/internal/decision"
	"stable/internal/sessionlog"
)

const (
	// summaryChunkChars bounds one provider call during iterative
	// summarization.
	summaryChunkChars = 6000
	// MaxCompactionInputChars bounds the total text fed to the summarizer
	// in one compaction round. Larger covered ranges compact their newest
	// part and leave the rest for the next round.
	MaxCompactionInputChars = 48000
	// summaryMaxChars rejects degenerate summaries.
	summaryMaxChars = 8000
	// summaryReserveChars is the budget reservation for a summary before it
	// exists; the summarizer is instructed to stay under 4000 characters.
	summaryReserveChars = 4000
)

const summaryPrompt = "Summarize faithfully and concisely. Preserve user intent, decisions, unresolved questions, and important IDs. Keep the running summary under 4000 characters."

// summarize turns covered projection items into a compaction boundary and
// the summary message that replaces them. When the covered range exceeds
// the input cap, only its newest part is summarized and the boundary range
// starts at the first item actually fed to the provider.
func (m *Manager) summarize(ctx context.Context, covered []sessionlog.Item) (sessionlog.Boundary, decision.ChatMessage, error) {
	if len(covered) == 0 {
		return sessionlog.Boundary{}, decision.ChatMessage{}, errors.New("not enough history to compact")
	}
	start := 0
	total := 0
	for i := len(covered) - 1; i >= 0; i-- {
		cost := len(itemText(covered[i]))
		if total+cost > MaxCompactionInputChars && i > start {
			start = i + 1
			break
		}
		total += cost
	}
	covered = covered[start:]
	var chunks []string
	var chunk strings.Builder
	for _, item := range covered {
		line := itemText(item)
		if line == "" {
			continue
		}
		for _, r := range line {
			if chunk.Len()+len(string(r)) > summaryChunkChars {
				chunks = append(chunks, chunk.String())
				chunk.Reset()
			}
			chunk.WriteRune(r)
		}
	}
	if chunk.Len() > 0 {
		chunks = append(chunks, chunk.String())
	}
	if len(chunks) == 0 {
		return sessionlog.Boundary{}, decision.ChatMessage{}, errors.New("no messages available to summarize")
	}
	answer := ""
	var err error
	for _, part := range chunks {
		input := part
		if answer != "" {
			input = "Current concise summary:\n" + answer + "\n\nNew conversation segment:\n" + part
		}
		answer, err = m.provider.GenerateChat(ctx, []decision.ChatMessage{{Role: "system", Content: summaryPrompt}, {Role: "user", Content: input}})
		if err != nil {
			return sessionlog.Boundary{}, decision.ChatMessage{}, fmt.Errorf("context summary failed: %w", err)
		}
		if answer == "" || len(answer) > summaryMaxChars {
			return sessionlog.Boundary{}, decision.ChatMessage{}, errors.New("context summary was empty or exceeded its safe size")
		}
	}
	boundary := sessionlog.Boundary{
		FromSeq: covered[0].Seq,
		ToSeq:   covered[len(covered)-1].Seq,
		Summary: answer,
		Scope:   sessionlog.BoundaryScopeSession,
	}
	message := decision.ChatMessage{Role: "assistant", Content: "Earlier conversation summary: " + answer}
	return boundary, message, nil
}

// itemText renders one projection item as summarizer input.
func itemText(item sessionlog.Item) string {
	switch item.Kind {
	case sessionlog.ItemMessage:
		return fmt.Sprintf("%s: %s\n", item.Message.Role, item.Message.Text)
	case sessionlog.ItemSummary:
		return "earlier summary: " + item.Summary.Summary + "\n"
	case sessionlog.ItemToolCall:
		return fmt.Sprintf("%s called tool %s\n", item.Call.CallID, item.Call.Name)
	case sessionlog.ItemToolResult:
		text := fmt.Sprintf("%v", item.Result.Result)
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		if item.Result.Error != "" {
			return fmt.Sprintf("tool %s failed: %s\n", item.Result.CallID, item.Result.Error)
		}
		return fmt.Sprintf("tool %s returned %s\n", item.Result.CallID, text)
	}
	return ""
}
