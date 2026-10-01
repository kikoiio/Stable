package prompt

import (
	"stable/internal/decision"
	"strings"
)

// ApproxTokens is a conservative, provider-neutral estimate for UTF-8 text.
func ApproxTokens(messages []decision.ChatMessage) int {
	n := 0
	for _, m := range messages {
		n += (len(m.Content)+3)/4 + 4
	}
	return n
}

func Fit(messages []decision.ChatMessage, budget int) []decision.ChatMessage {
	if budget <= 0 || len(messages) < 2 {
		return messages
	}
	result := append([]decision.ChatMessage(nil), messages[:1]...)
	used := ApproxTokens(result)
	var tail []decision.ChatMessage
	for i := len(messages) - 1; i >= 1; i-- {
		cost := ApproxTokens(messages[i : i+1])
		if used+cost > budget {
			if i == len(messages)-1 && used+4 < budget {
				clipped := messages[i]
				clipped.Content = truncateForBudget(clipped.Content, budget-used-4)
				if clipped.Content != "" {
					tail = append(tail, clipped)
				}
			}
			break
		}
		tail = append(tail, messages[i])
		used += cost
	}
	for i := len(tail) - 1; i >= 0; i-- {
		result = append(result, tail[i])
	}
	return pairToolMessages(result)
}

func BudgetToolResult(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	marker := []rune("\n[tool result truncated for model context]\n")
	if len(marker) >= maxChars {
		return string(marker[:maxChars])
	}
	keep := maxChars - len(marker)
	head := keep / 2
	tail := keep - head
	return string(runes[:head]) + string(marker) + strings.TrimLeft(string(runes[len(runes)-tail:]), " ")
}

func truncateForBudget(text string, tokens int) string {
	marker := " [message truncated for model context]"
	maxBytes := tokens*4 - len(marker)
	if maxBytes < 1 {
		return ""
	}
	r := []rune(text)
	for len(r) > 0 && len(string(r)) > maxBytes {
		r = r[:len(r)-1]
	}
	return string(r) + marker
}
func pairToolMessages(messages []decision.ChatMessage) []decision.ChatMessage {
	calls := map[string]bool{}
	for _, m := range messages {
		if id := toolID(m.Content, "[recorded tool call "); id != "" {
			calls[id] = true
		}
	}
	out := messages[:0]
	for _, m := range messages {
		if id := toolID(m.Content, "[recorded tool result "); id != "" && !calls[id] {
			continue
		}
		out = append(out, m)
	}
	return out
}
func toolID(text, prefix string) string {
	if !strings.HasPrefix(text, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(text, prefix)
	for i, r := range rest {
		if r == ' ' || r == ':' {
			return rest[:i]
		}
	}
	return ""
}
