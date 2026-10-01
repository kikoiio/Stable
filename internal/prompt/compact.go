package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"stable/internal/decision"
	"stable/internal/sessionlog"
	"strings"
)

type Summarizer interface {
	GenerateChat(context.Context, []decision.ChatMessage) (string, error)
}

func Compact(ctx context.Context, root, id string, events []sessionlog.Event, keep int, provider Summarizer) (sessionlog.Event, error) {
	if provider == nil {
		return sessionlog.Event{}, errors.New("context exceeds budget and no summarizer is configured")
	}
	if keep < 1 {
		keep = 8
	}
	cut := len(events) - keep
	if cut <= 0 {
		return sessionlog.Event{}, errors.New("not enough history to compact")
	}
	if events[cut].Type == sessionlog.EventToolResult {
		var result sessionlog.ToolResult
		raw, _ := json.Marshal(events[cut].Data)
		_ = json.Unmarshal(raw, &result)
		for i := cut - 1; i >= 0; i-- {
			if events[i].Type == sessionlog.EventToolCall {
				var call sessionlog.ToolCall
				b, _ := json.Marshal(events[i].Data)
				_ = json.Unmarshal(b, &call)
				if call.CallID == result.CallID {
					cut = i
					break
				}
			}
		}
	}
	var chunks []string
	var chunk strings.Builder
	for _, e := range events[:cut] {
		if e.Type != sessionlog.EventMessage {
			continue
		}
		var message sessionlog.Message
		raw, _ := json.Marshal(e.Data)
		if json.Unmarshal(raw, &message) != nil {
			continue
		}
		line := fmt.Sprintf("%s: %s\n", message.Role, message.Text)
		for _, r := range line {
			if chunk.Len()+len(string(r)) > 6000 {
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
		return sessionlog.Event{}, errors.New("no messages available to summarize")
	}
	answer := ""
	var err error
	for _, part := range chunks {
		input := part
		if answer != "" {
			input = "Current concise summary:\n" + answer + "\n\nNew conversation segment:\n" + part
		}
		answer, err = provider.GenerateChat(ctx, []decision.ChatMessage{{Role: "system", Content: "Summarize faithfully and concisely. Preserve user intent, decisions, unresolved questions, and important IDs. Keep the running summary under 4000 characters."}, {Role: "user", Content: input}})
		if err != nil {
			return sessionlog.Event{}, fmt.Errorf("context summary failed: %w", err)
		}
		if answer == "" || len(answer) > 8000 {
			return sessionlog.Event{}, errors.New("context summary was empty or exceeded its safe size")
		}
	}
	boundary, err := sessionlog.Append(root, id, sessionlog.EventBoundary, sessionlog.Boundary{FromSeq: events[0].Seq, ToSeq: events[cut-1].Seq, Summary: answer})
	if err != nil {
		return sessionlog.Event{}, err
	}
	// Re-append the retained tail after the boundary. The original source events
	// remain untouched before it, while replay after this point is self-contained.
	for _, event := range events[cut:] {
		switch event.Type {
		case sessionlog.EventMessage, sessionlog.EventProposal, sessionlog.EventToolCall, sessionlog.EventToolResult:
			if _, err = sessionlog.Append(root, id, event.Type, event.Data); err != nil {
				return sessionlog.Event{}, err
			}
		}
	}
	return boundary, nil
}
