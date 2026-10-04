package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

type compatibleProvider struct{ config Config }

func newCompatible(c Config) Provider { return compatibleProvider{config: c} }

func (p compatibleProvider) Stream(ctx context.Context, request Request) (<-chan Event, <-chan error) {
	return streamChannels(ctx, func(send func(Event) error) error {
		if p.config.Model.BaseURL == "" {
			return &ProviderError{Class: ErrorProvider, Message: "OpenAI-compatible base URL is missing"}
		}
		messages := make([]map[string]any, 0, len(request.Messages))
		for _, message := range request.Messages {
			if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
				return errors.New("unsupported message role")
			}
			if message.Content != "" || len(message.ToolUses) > 0 || len(message.ToolResults) == 0 {
				entry := map[string]any{"role": message.Role, "content": message.Content}
				if len(message.ToolUses) > 0 {
					calls := make([]map[string]any, 0, len(message.ToolUses))
					for _, tool := range message.ToolUses {
						arguments := string(tool.Arguments)
						if arguments == "" {
							arguments = "{}"
						}
						calls = append(calls, map[string]any{"id": tool.ID, "type": "function", "function": map[string]any{"name": tool.Name, "arguments": arguments}})
					}
					entry["tool_calls"] = calls
				}
				messages = append(messages, entry)
			}
			for _, result := range message.ToolResults {
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": result.ToolUseID, "content": result.Content})
			}
		}
		body := map[string]any{"model": chooseModel(request.Model, p.config.Model.Model), "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
		if len(request.Tools) > 0 {
			tools := make([]map[string]any, 0, len(request.Tools))
			for _, tool := range request.Tools {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
			}
			body["tools"] = tools
		}
		calls := map[int]ToolCall{}
		started := map[int]bool{}
		finishReason := ""
		return postSSE(ctx, p.config.HTTPClient, endpoint(p.config.Model.BaseURL, "/chat/completions"), body, map[string]string{"Authorization": "Bearer " + p.config.Model.APIKey}, func(frame sseFrame) error {
			if frame.Data == "[DONE]" {
				return send(Event{Kind: StreamEnd, StopReason: finishReason})
			}
			var payload struct {
				Choices []struct {
					Index int `json:"index"`
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     *int `json:"prompt_tokens"`
					CompletionTokens *int `json:"completion_tokens"`
					PromptDetails    *struct {
						CachedTokens *int `json:"cached_tokens"`
					} `json:"prompt_tokens_details"`
				} `json:"usage"`
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				return errors.New("invalid OpenAI-compatible stream event")
			}
			if len(payload.Error) > 0 && string(payload.Error) != "null" {
				return &ProviderError{Class: ErrorProvider, Message: "OpenAI-compatible stream returned an error"}
			}
			for _, choice := range payload.Choices {
				if choice.Delta.Content != "" {
					if err := send(Event{Kind: TextDelta, Text: choice.Delta.Content}); err != nil {
						return err
					}
				}
				if choice.Delta.ReasoningContent != "" {
					if err := send(Event{Kind: ThinkingDelta, Text: choice.Delta.ReasoningContent}); err != nil {
						return err
					}
				}
				for _, delta := range choice.Delta.ToolCalls {
					call := calls[delta.Index]
					if delta.ID != "" {
						call.ID = delta.ID
					}
					if delta.Function.Name != "" {
						call.Name = delta.Function.Name
					}
					if delta.Function.Arguments != "" {
						call.Arguments = append(call.Arguments, []byte(delta.Function.Arguments)...)
					}
					calls[delta.Index] = call
					if !started[delta.Index] && call.Name != "" {
						started[delta.Index] = true
						if err := send(Event{Kind: ToolCallStart, Tool: &call}); err != nil {
							return err
						}
					}
					if delta.Function.Arguments != "" {
						if err := send(Event{Kind: ToolCallDelta, Text: delta.Function.Arguments, Tool: &call}); err != nil {
							return err
						}
					}
				}
				if choice.FinishReason != nil {
					finishReason = *choice.FinishReason
					for index, call := range calls {
						if !json.Valid(call.Arguments) {
							return fmt.Errorf("invalid tool arguments at index %s", strconv.Itoa(index))
						}
						call.Complete = true
						calls[index] = call
						if err := send(Event{Kind: ToolCallComplete, Tool: &call}); err != nil {
							return err
						}
					}
				}
			}
			if payload.Usage != nil {
				var cached *int
				if payload.Usage.PromptDetails != nil {
					cached = payload.Usage.PromptDetails.CachedTokens
				}
				usage := normalizeOpenAIUsage(payload.Usage.PromptTokens, payload.Usage.CompletionTokens, cached)
				if usage.InputTokens != nil || usage.OutputTokens != nil || usage.CacheReadTokens != nil {
					return send(Event{Kind: Usage, Usage: &usage})
				}
			}
			return nil
		})
	})
}
