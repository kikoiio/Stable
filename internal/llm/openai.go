package llm

import (
	"context"
	"encoding/json"
	"errors"
)

type openAIProvider struct{ config Config }

func newOpenAI(c Config) Provider { return openAIProvider{config: c} }

func (p openAIProvider) Stream(ctx context.Context, request Request) (<-chan Event, <-chan error) {
	return streamChannels(ctx, func(send func(Event) error) error {
		input := make([]map[string]any, 0, len(request.Messages))
		for _, message := range request.Messages {
			if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
				return errors.New("unsupported message role")
			}
			if message.Content != "" || (len(message.ToolUses) == 0 && len(message.ToolResults) == 0) {
				input = append(input, map[string]any{"role": message.Role, "content": message.Content})
			}
			for _, tool := range message.ToolUses {
				arguments := string(tool.Arguments)
				if arguments == "" {
					arguments = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": tool.ID, "name": tool.Name, "arguments": arguments})
			}
			for _, result := range message.ToolResults {
				input = append(input, map[string]any{"type": "function_call_output", "call_id": result.ToolUseID, "output": result.Content})
			}
		}
		maxTokens := request.MaxTokens
		if maxTokens <= 0 {
			maxTokens = p.config.MaxTokens
		}
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body := map[string]any{"model": chooseModel(request.Model, p.config.Model.Model), "input": input, "stream": true, "store": false, "max_output_tokens": maxTokens}
		if len(request.Tools) > 0 {
			tools := make([]map[string]any, 0, len(request.Tools))
			for _, tool := range request.Tools {
				tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema})
			}
			body["tools"] = tools
		}
		key := p.config.Model.APIKey
		if key == "" {
			return &ProviderError{Class: ErrorAuth, Message: "OpenAI API key is missing"}
		}
		var base string
		if p.config.Model.BaseURL != "" {
			base = p.config.Model.BaseURL
		} else {
			base = "https://api.openai.com/v1"
		}
		calls := map[string]ToolCall{}
		items := map[string]string{}
		var thinking string
		return postSSE(ctx, p.config.HTTPClient, endpoint(base, "/responses"), body, map[string]string{"Authorization": "Bearer " + key}, func(frame sseFrame) error {
			if frame.Data == "[DONE]" {
				return send(Event{Kind: StreamEnd, StopReason: "end_turn"})
			}
			var payload struct {
				Type   string `json:"type"`
				Delta  string `json:"delta"`
				ItemID string `json:"item_id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Item   struct {
					Type   string `json:"type"`
					ID     string `json:"id"`
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"item"`
				Response struct {
					Usage struct {
						InputTokens  *int `json:"input_tokens"`
						OutputTokens *int `json:"output_tokens"`
						InputDetails struct {
							CachedTokens *int `json:"cached_tokens"`
						} `json:"input_tokens_details"`
					} `json:"usage"`
					Status string `json:"status"`
				} `json:"response"`
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				return errors.New("invalid OpenAI stream event")
			}
			switch payload.Type {
			case "response.output_text.delta":
				return send(Event{Kind: TextDelta, Text: payload.Delta})
			case "response.reasoning_summary_text.delta":
				thinking += payload.Delta
				return send(Event{Kind: ThinkingDelta, Text: payload.Delta})
			case "response.reasoning_summary_text.done":
				text := thinking
				thinking = ""
				return send(Event{Kind: ThinkingComplete, Text: text})
			case "response.output_item.added":
				if payload.Item.Type == "function_call" {
					id := payload.Item.CallID
					if id == "" {
						id = payload.Item.ID
					}
					call := ToolCall{ID: id, Name: payload.Item.Name}
					calls[id], items[payload.Item.ID] = call, id
					return send(Event{Kind: ToolCallStart, Tool: &call})
				}
			case "response.function_call_arguments.delta":
				id := items[payload.ItemID]
				call := calls[id]
				call.Arguments = append(call.Arguments, []byte(payload.Delta)...)
				calls[id] = call
				return send(Event{Kind: ToolCallDelta, Text: payload.Delta, Tool: &call})
			case "response.function_call_arguments.done":
				id := items[payload.ItemID]
				call := calls[id]
				if !json.Valid(call.Arguments) {
					return errors.New("invalid OpenAI tool arguments")
				}
				call.Complete = true
				calls[id] = call
				return send(Event{Kind: ToolCallComplete, Tool: &call})
			case "response.completed":
				usage := normalizeOpenAIUsage(payload.Response.Usage.InputTokens, payload.Response.Usage.OutputTokens, payload.Response.Usage.InputDetails.CachedTokens)
				if usage.InputTokens != nil || usage.OutputTokens != nil || usage.CacheReadTokens != nil {
					if err := send(Event{Kind: Usage, Usage: &usage}); err != nil {
						return err
					}
				}
				return send(Event{Kind: StreamEnd, StopReason: payload.Response.Status, Usage: &usage})
			case "response.failed", "error":
				return &ProviderError{Class: ErrorProvider, Message: "OpenAI stream returned an error"}
			}
			return nil
		})
	})
}

func normalizeOpenAIUsage(input, output, cached *int) UsageInfo {
	usage := UsageInfo{OutputTokens: output, CacheReadTokens: cached}
	if input != nil {
		n := *input
		usage.InputTokens = &n
	}
	return usage
}
