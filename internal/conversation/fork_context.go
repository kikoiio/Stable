package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"stable/internal/llm"
	"stable/internal/sessioncontext"
	"stable/internal/sessionlog"
)

// forkContextBuilder reconstructs the visible conversation from the session
// event log. It intentionally projects only user/assistant text and completed
// tool exchanges; model thinking and other session-owned records never enter a
// fork's context.
type forkContextBuilder struct {
	projectRoot         string
	contextWindowTokens int
}

// NewForkContextSource returns a session-backed context source. A zero or
// invalid context window uses the same effective-window fallback as normal
// conversation runs.
func NewForkContextSource(projectRoot string, contextWindowTokens int) ForkContextSource {
	return forkContextBuilder{projectRoot: projectRoot, contextWindowTokens: contextWindowTokens}
}

// Build returns either no context, the most recent five visible conversation
// rounds, or the full visible transcript that fits the configured input
// budget. parentRunID is accepted so callers can bind context acquisition to a
// parent run; transcript visibility itself is session-scoped and event-ordered.
func (b forkContextBuilder) Build(ctx context.Context, sessionID, parentRunID string, mode ForkContextMode) ([]llm.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch mode {
	case ForkContextNone:
		return []llm.Message{}, nil
	case ForkContextRecent, ForkContextFull:
	default:
		return nil, fmt.Errorf("unknown fork context mode %q", mode)
	}
	if sessionID == "" {
		return nil, errors.New("fork context requires a session ID")
	}
	transcript, err := sessionlog.Replay(b.projectRoot, sessionID)
	if err != nil {
		return nil, fmt.Errorf("replay fork context: %w", err)
	}
	rounds := forkVisibleRounds(transcript.Events)
	if mode == ForkContextRecent && len(rounds) > 5 {
		rounds = rounds[len(rounds)-5:]
	}
	return fitForkRounds(rounds, forkInputBudget(b.contextWindowTokens))
}

// forkVisibleRounds groups a user turn and the visible assistant/tool output
// that follows it. Consecutive user messages begin distinct rounds. Assistant
// text before the first user message (for example, an imported summary) is
// kept as its own visible round.
func forkVisibleRounds(events []sessionlog.Event) [][]llm.Message {
	covered, boundarySeq := sessionlog.CoveredSeqs(events)
	matchedCalls := make(map[string]bool)
	for _, event := range events {
		if covered[event.Seq] {
			continue
		}
		switch event.Type {
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) == nil && call.CallID != "" {
				matchedCalls[call.CallID] = false
			}
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) == nil && result.CallID != "" {
				if _, exists := matchedCalls[result.CallID]; exists {
					matchedCalls[result.CallID] = true
				}
			}
		}
	}

	var rounds [][]llm.Message
	newRound := func() { rounds = append(rounds, []llm.Message{}) }
	for _, event := range events {
		if covered[event.Seq] {
			continue
		}
		if event.Type == sessionlog.EventBoundary {
			if event.Seq != boundarySeq {
				continue
			}
			var boundary sessionlog.Boundary
			if decodeSessionData(event.Data, &boundary) == nil && boundary.Summary != "" {
				if len(rounds) == 0 {
					newRound()
				}
				last := len(rounds) - 1
				rounds[last] = append(rounds[last], llm.Message{Role: "assistant", Content: "Earlier conversation summary: " + boundary.Summary})
			}
			continue
		}
		switch event.Type {
		case sessionlog.EventMessage:
			var message sessionlog.Message
			if decodeSessionData(event.Data, &message) != nil || message.Text == "" {
				continue
			}
			if message.Role == "user" && forkVisibleMessageKind(message.Kind) {
				newRound()
				rounds[len(rounds)-1] = append(rounds[len(rounds)-1], llm.Message{Role: "user", Content: message.Text})
			} else if message.Role == "assistant" && forkVisibleMessageKind(message.Kind) {
				if len(rounds) == 0 {
					newRound()
				}
				rounds[len(rounds)-1] = append(rounds[len(rounds)-1], llm.Message{Role: "assistant", Content: message.Text})
			}
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeSessionData(event.Data, &call) != nil || call.CallID == "" || !matchedCalls[call.CallID] {
				continue
			}
			if len(rounds) == 0 {
				newRound()
			}
			input, marshalErr := json.Marshal(call.Input)
			tool := llm.ToolUse{ID: call.CallID, Name: call.Name}
			if marshalErr == nil && string(input) != "null" {
				tool.Arguments = input
			}
			rounds[len(rounds)-1] = append(rounds[len(rounds)-1], llm.Message{Role: "assistant", ToolUses: []llm.ToolUse{tool}})
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) != nil || !matchedCalls[result.CallID] {
				continue
			}
			if len(rounds) == 0 {
				newRound()
			}
			content, ok := result.Result.(string)
			if !ok {
				encoded, marshalErr := json.Marshal(result.Result)
				if marshalErr != nil {
					continue
				}
				content = string(encoded)
			}
			rounds[len(rounds)-1] = append(rounds[len(rounds)-1], llm.Message{Role: "user", ToolResults: []llm.ToolResultPart{{ToolUseID: result.CallID, Content: content, IsError: result.Error != ""}}})
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if decodeSessionData(event.Data, &runEvent) != nil || runEvent.Kind != "text_delta" {
				continue
			}
			var payload struct {
				Text string `json:"text"`
			}
			if decodeSessionData(runEvent.Payload, &payload) != nil || payload.Text == "" {
				continue
			}
			if len(rounds) == 0 {
				newRound()
			}
			last := len(rounds) - 1
			if n := len(rounds[last]); n > 0 && rounds[last][n-1].Role == "assistant" && len(rounds[last][n-1].ToolUses) == 0 {
				rounds[last][n-1].Content += payload.Text
			} else {
				rounds[last] = append(rounds[last], llm.Message{Role: "assistant", Content: payload.Text})
			}
		}
	}
	return rounds
}

func forkVisibleMessageKind(kind string) bool {
	// Empty kind is retained for old logs. Current visible chat messages use
	// "text"; goal requests are visible user intent too. Other kinds can carry
	// internal thought or UI metadata and are excluded.
	return kind == "" || kind == "text" || kind == "goal_request"
}

func flattenForkRounds(rounds [][]llm.Message) []llm.Message {
	var messages []llm.Message
	for _, round := range rounds {
		messages = append(messages, round...)
	}
	return messages
}

func forkInputBudget(configuredWindow int) int {
	window, _ := sessioncontext.EffectiveWindow(configuredWindow)
	// Match the normal session context manager's output and safety reserves.
	return int(float64(window) * (1 - sessioncontext.OutputReserveRatio - sessioncontext.SafetyMarginRatio))
}

func fitForkRounds(rounds [][]llm.Message, budget int) ([]llm.Message, error) {
	if budget <= 0 {
		return nil, errors.New("fork context budget must be positive")
	}
	start := 0
	for start < len(rounds) && forkApproxTokens(flattenForkRounds(rounds[start:])) > budget {
		start++
	}
	if start == len(rounds) && len(rounds) > 0 {
		return nil, errors.New("latest visible conversation round exceeds fork context budget")
	}
	return flattenForkRounds(rounds[start:]), nil
}

func forkApproxTokens(messages []llm.Message) int {
	tokens := 0
	for _, message := range messages {
		size := len(message.Content)
		for _, use := range message.ToolUses {
			size += len(use.Name) + len(use.Arguments)
		}
		for _, result := range message.ToolResults {
			size += len(result.Content)
		}
		tokens += (size+3)/4 + 4
	}
	return tokens
}
