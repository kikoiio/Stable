package prompt

import (
	"encoding/json"
	"fmt"

	"stable/internal/decision"
	"stable/internal/sessionlog"
)

func Project(replay sessionlog.Transcript, selectedGoal string) []decision.ChatMessage {
	start := 0
	var summary string
	for i, e := range replay.Events {
		if e.Type == sessionlog.EventBoundary {
			var b sessionlog.Boundary
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &b) == nil {
				summary = b.Summary
				start = i + 1
			}
		}
	}
	out := []decision.ChatMessage{{Role: "system", Content: StableSystem(), Cache: true}}
	if selectedGoal != "" {
		out = append(out, decision.ChatMessage{Role: "system", Content: "Selected global goal context: " + selectedGoal})
	}
	if summary != "" {
		out = append(out, decision.ChatMessage{Role: "assistant", Content: "Earlier conversation summary: " + summary})
	}
	for _, e := range replay.Events[start:] {
		switch e.Type {
		case sessionlog.EventMessage:
			var m sessionlog.Message
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			role := m.Role
			if role == "agent" {
				role = "assistant"
			}
			if role != "user" && role != "assistant" {
				continue
			}
			out = append(out, decision.ChatMessage{Role: role, Content: m.Text})
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &call) == nil {
				input, _ := json.Marshal(call.Input)
				out = append(out, decision.ChatMessage{Role: "assistant", Content: fmt.Sprintf("[recorded tool call %s: %s %s]", call.CallID, call.Name, input)})
			}
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &result) == nil {
				value, _ := json.Marshal(result.Result)
				limited := BudgetToolResult(string(value), 8000)
				out = append(out, decision.ChatMessage{Role: "user", Content: fmt.Sprintf("[recorded tool result %s: %s %s]", result.CallID, result.Error, limited)})
			}
		}
	}
	return out
}
