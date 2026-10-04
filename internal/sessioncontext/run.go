package sessioncontext

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"stable/internal/agent"
	"stable/internal/llm"
)

// PrepareRun implements agent.ContextPreparer. It compacts the oldest
// completed run messages once the estimate crosses the trigger, protects
// complete tool use/result exchanges, and returns the boundary the Runner
// publishes before sending the compacted request. System messages always
// stay at the head. Any failure returns a visible error so the run never
// sends an unprocessed over-budget request.
func (m *Manager) PrepareRun(ctx context.Context, runID string, messages []llm.Message, msgSeqs []uint64) (agent.PreparedRun, error) {
	if len(messages) != len(msgSeqs) {
		return agent.PreparedRun{}, errors.New("message sequence alignment is broken")
	}
	if approxRunTokens(messages) <= m.triggerTokens() {
		return agent.PreparedRun{Messages: messages, TailStart: 0}, nil
	}
	if m.provider == nil {
		return agent.PreparedRun{}, errors.New("context exceeds budget and no summarizer is configured")
	}
	head := 0
	for head < len(messages) && messages[head].Role == "system" {
		head++
	}
	cut, err := m.selectRunCut(messages, head)
	if err != nil {
		return agent.PreparedRun{}, err
	}
	boundary, summary, err := m.summarizeRun(ctx, runID, messages[head:cut], msgSeqs[head:cut])
	if err != nil {
		return agent.PreparedRun{}, err
	}
	out := append(append([]llm.Message{}, messages[:head]...), summary)
	out = append(out, messages[cut:]...)
	if approxRunTokens(out) > m.inputBudget() {
		return agent.PreparedRun{}, errors.New("context remains over budget after compaction")
	}
	return agent.PreparedRun{Messages: out, HeadKept: head, TailStart: cut, Boundary: &boundary}, nil
}

// selectRunCut picks the oldest coverage under the input budget without
// splitting a tool use/result exchange.
func (m *Manager) selectRunCut(messages []llm.Message, head int) (int, error) {
	if len(messages)-head < 2 {
		return 0, errors.New("not enough history to compact")
	}
	reserve := (summaryReserveChars+3)/4 + 4
	budget := m.inputBudget() - approxRunTokens(messages[:head]) - reserve
	if budget < 0 {
		return 0, errors.New("system prefix alone exceeds the context budget")
	}
	cut := 0
	for i := head + 1; i < len(messages); i++ {
		if approxRunTokens(messages[i:]) <= budget {
			cut = i
			break
		}
	}
	if cut == 0 {
		return 0, errors.New("context tail alone exceeds the budget")
	}
	for cut < len(messages) && len(messages[cut].ToolResults) > 0 && len(messages[cut-1].ToolUses) > 0 {
		cut++
	}
	if cut-head < 1 {
		return 0, errors.New("not enough completed history to compact")
	}
	return cut, nil
}

// summarizeRun condenses the covered messages and maps the coverage onto
// the run sequence range the boundary will replace. Sequences of 0 belong
// to history that predates the run and has no events to cover.
func (m *Manager) summarizeRun(ctx context.Context, runID string, covered []llm.Message, coveredSeqs []uint64) (agent.ContextBoundary, llm.Message, error) {
	start := 0
	total := 0
	for i := len(covered) - 1; i >= 0; i-- {
		cost := len(runMessageText(covered[i]))
		if total+cost > MaxCompactionInputChars && i > start {
			start = i + 1
			break
		}
		total += cost
	}
	covered, coveredSeqs = covered[start:], coveredSeqs[start:]
	var text strings.Builder
	for _, message := range covered {
		text.WriteString(runMessageText(message))
	}
	answer, err := m.summarizeText(ctx, text.String())
	if err != nil {
		return agent.ContextBoundary{}, llm.Message{}, err
	}
	toSeq := coveredSeqs[len(coveredSeqs)-1]
	if toSeq == 0 {
		return agent.ContextBoundary{}, llm.Message{}, errors.New("covered history predates this run and cannot be bounded")
	}
	fromSeq := coveredSeqs[0]
	if fromSeq == 0 {
		fromSeq = 1
	}
	boundary := agent.ContextBoundary{RunID: runID, FromSeq: fromSeq, ToSeq: toSeq, Summary: answer}
	return boundary, llm.Message{Role: "assistant", Content: "Earlier conversation summary: " + answer}, nil
}

// approxRunTokens is a conservative, provider-neutral estimate matching
// prompt.ApproxTokens.
func approxRunTokens(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		size := len(m.Content)
		for _, use := range m.ToolUses {
			size += len(use.Name) + len(use.Arguments)
		}
		for _, result := range m.ToolResults {
			size += len(result.Content)
		}
		n += (size+3)/4 + 4
	}
	return n
}

// runMessageText renders one run message as summarizer input.
func runMessageText(m llm.Message) string {
	var b strings.Builder
	if m.Content != "" {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	for _, use := range m.ToolUses {
		fmt.Fprintf(&b, "%s called tool %s %s\n", m.Role, use.Name, clipText(string(use.Arguments), 200))
	}
	for _, result := range m.ToolResults {
		prefix := "tool result"
		if result.IsError {
			prefix = "tool error"
		}
		fmt.Fprintf(&b, "%s %s: %s\n", prefix, result.ToolUseID, clipText(result.Content, 400))
	}
	return b.String()
}

func clipText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}
