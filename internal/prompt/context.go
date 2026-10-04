package prompt

import (
	"encoding/json"
	"fmt"

	"stable/internal/decision"
	"stable/internal/sessionlog"
)

// SystemPrefix builds the fixed system messages every provider request
// starts with.
func SystemPrefix(selectedGoal string) []decision.ChatMessage {
	out := []decision.ChatMessage{{Role: "system", Content: StableSystem(), Cache: true}}
	if selectedGoal != "" {
		out = append(out, decision.ChatMessage{Role: "system", Content: "Selected global goal context: " + selectedGoal})
	}
	return out
}

// Project builds provider messages from a replayed transcript through the
// unified session projection, so the model sees exactly what the
// transcript shows: the latest boundary summary plus the uncovered tail.
func Project(replay sessionlog.Transcript, selectedGoal string) []decision.ChatMessage {
	projection := sessionlog.Project(replay)
	return append(SystemPrefix(selectedGoal), MessagesFromItems(projection.Items)...)
}

// MessagesFromItems renders projection items as provider messages. A
// trailing unmatched tool call is dropped: it stays auditable in the log
// but must not become the recoverable tail of a normal conversation.
func MessagesFromItems(items []sessionlog.Item) []decision.ChatMessage {
	end := len(items)
	for end > 0 && items[end-1].Kind == sessionlog.ItemToolCall && !items[end-1].Matched {
		end--
	}
	out := []decision.ChatMessage{}
	for _, item := range items[:end] {
		switch item.Kind {
		case sessionlog.ItemMessage:
			m := item.Message
			role := m.Role
			if role == "agent" {
				role = "assistant"
			}
			if role != "user" && role != "assistant" {
				continue
			}
			out = append(out, decision.ChatMessage{Role: role, Content: m.Text})
		case sessionlog.ItemSummary:
			out = append(out, decision.ChatMessage{Role: "assistant", Content: "Earlier conversation summary: " + item.Summary.Summary})
		case sessionlog.ItemToolCall:
			input, _ := json.Marshal(item.Call.Input)
			out = append(out, decision.ChatMessage{Role: "assistant", Content: fmt.Sprintf("[recorded tool call %s: %s %s]", item.Call.CallID, item.Call.Name, input)})
		case sessionlog.ItemToolResult:
			value, _ := json.Marshal(item.Result.Result)
			limited := BudgetToolResult(string(value), 8000)
			out = append(out, decision.ChatMessage{Role: "user", Content: fmt.Sprintf("[recorded tool result %s: %s %s]", item.Result.CallID, item.Result.Error, limited)})
		}
	}
	return out
}
