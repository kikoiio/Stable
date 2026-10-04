package sessioncontext

import (
	"context"
	"errors"

	"stable/internal/decision"
	"stable/internal/prompt"
	"stable/internal/sessionlog"
)

// Prepared carries the provider messages for one turn and, when compaction
// was required, the boundary the caller must persist before sending.
type Prepared struct {
	Messages  []decision.ChatMessage
	Boundary  *sessionlog.Boundary
	Compacted bool
}

// Prepare builds provider messages from the unified session projection.
// When the estimate crosses the trigger it summarizes the oldest completed
// range, protects complete tool call/result groups, and returns the summary
// plus the intact tail. Any failure — summarization, range selection, or a
// result that would still exceed the budget — returns a visible error and
// no messages, so the caller never sends an unprocessed over-budget
// request. The boundary is returned, not appended: the caller stays the
// only session log writer and reports persistence failures the same way.
func (m *Manager) Prepare(ctx context.Context, projection sessionlog.Projection, prefix []decision.ChatMessage) (Prepared, error) {
	messages := append(append([]decision.ChatMessage{}, prefix...), prompt.MessagesFromItems(projection.Items)...)
	if prompt.ApproxTokens(messages) <= m.triggerTokens() {
		return Prepared{Messages: messages}, nil
	}
	if m.provider == nil {
		return Prepared{}, errors.New("context exceeds budget and no summarizer is configured")
	}
	cut, err := m.selectCut(projection.Items, prefix)
	if err != nil {
		return Prepared{}, err
	}
	boundary, summary, err := m.summarize(ctx, projection.Items[:cut])
	if err != nil {
		return Prepared{}, err
	}
	messages = append(append([]decision.ChatMessage{}, prefix...), summary)
	messages = append(messages, prompt.MessagesFromItems(projection.Items[cut:])...)
	if prompt.ApproxTokens(messages) > m.inputBudget() {
		return Prepared{}, errors.New("context remains over budget after compaction")
	}
	return Prepared{Messages: messages, Boundary: &boundary, Compacted: true}, nil
}

// selectCut picks the oldest coverage that brings prefix + summary + tail
// under the input budget. The cut never splits a tool call/result pair and
// never covers an unmatched call.
func (m *Manager) selectCut(items []sessionlog.Item, prefix []decision.ChatMessage) (int, error) {
	if len(items) < 2 {
		return 0, errors.New("not enough history to compact")
	}
	// Reserve the instructed summary size; Prepare re-verifies with the
	// actual summary before returning.
	reserve := prompt.ApproxTokens([]decision.ChatMessage{{Role: "assistant", Content: string(make([]byte, summaryReserveChars))}})
	budget := m.inputBudget() - prompt.ApproxTokens(prefix) - reserve
	if budget < 0 {
		return 0, errors.New("system prefix alone exceeds the context budget")
	}
	cut := 0
	for i := 1; i < len(items); i++ {
		if prompt.ApproxTokens(prompt.MessagesFromItems(items[i:])) <= budget {
			cut = i
			break
		}
	}
	if cut == 0 {
		return 0, errors.New("context tail alone exceeds the budget")
	}
	// A covered call whose result landed in the tail would split a pair;
	// pull such results into the covered range.
	coveredCalls := map[string]bool{}
	for _, item := range items[:cut] {
		if item.Kind == sessionlog.ItemToolCall {
			coveredCalls[item.Call.CallID] = true
		}
	}
	for cut < len(items) && items[cut].Kind == sessionlog.ItemToolResult && coveredCalls[items[cut].Result.CallID] {
		cut++
	}
	// An unmatched call can never be summarized: it has no result yet.
	for cut > 0 && items[cut-1].Kind == sessionlog.ItemToolCall && !items[cut-1].Matched {
		cut--
	}
	if cut == 0 {
		return 0, errors.New("not enough completed history to compact")
	}
	return cut, nil
}
