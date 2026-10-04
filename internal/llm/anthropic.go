package llm

import (
	"context"
	"encoding/json"
	"errors"
)

type anthropicProvider struct {
	config Config
}

func newAnthropic(c Config) Provider { return anthropicProvider{config: c} }

func (p anthropicProvider) Stream(ctx context.Context, request Request) (<-chan Event, <-chan error) {
	return streamChannels(ctx, func(send func(Event) error) error {
		messages := make([]map[string]any, 0, len(request.Messages))
		var system string
		for _, message := range request.Messages {
			if message.Role == "system" {
				system += message.Content + "\n"
				continue
			}
			if message.Role != "user" && message.Role != "assistant" {
				return errors.New("unsupported message role")
			}
			if len(message.ToolUses) == 0 && len(message.ToolResults) == 0 {
				messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
				continue
			}
			blocks := make([]map[string]any, 0, 1+len(message.ToolUses)+len(message.ToolResults))
			if message.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
			}
			for _, tool := range message.ToolUses {
				input := json.RawMessage(tool.Arguments)
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": input})
			}
			for _, result := range message.ToolResults {
				block := map[string]any{"type": "tool_result", "tool_use_id": result.ToolUseID, "content": result.Content}
				if result.IsError {
					block["is_error"] = true
				}
				blocks = append(blocks, block)
			}
			messages = append(messages, map[string]any{"role": message.Role, "content": blocks})
		}
		maxTokens := request.MaxTokens
		if maxTokens <= 0 {
			maxTokens = p.config.MaxTokens
		}
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body := map[string]any{"model": chooseModel(request.Model, p.config.Model.Model), "max_tokens": maxTokens, "stream": true, "messages": messages}
		if system != "" {
			body["system"] = system
		}
		if len(request.Tools) > 0 {
			body["tools"] = request.Tools
		}
		url := "https://api.anthropic.com/v1/messages"
		key := p.config.Model.APIKey
		if key == "" {
			return &ProviderError{Class: ErrorAuth, Message: "Anthropic API key is missing"}
		}
		var thinking string
		var usage UsageInfo
		tools := map[int]ToolCall{}
		client := p.config.HTTPClient
		return postSSE(ctx, client, url, body, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}, func(frame sseFrame) error {
			if frame.Data == "[DONE]" {
				return send(Event{Kind: StreamEnd, StopReason: "end_turn", Usage: &usage})
			}
			var payload struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
					StopReason  string `json:"stop_reason"`
				} `json:"delta"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
				Message struct {
					Usage struct {
						InputTokens         *int `json:"input_tokens"`
						CacheReadInput      *int `json:"cache_read_input_tokens"`
						CacheCreationTokens *int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					OutputTokens *int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
				return errors.New("invalid Anthropic stream event")
			}
			switch payload.Type {
			case "message_start":
				if payload.Message.Usage.InputTokens != nil {
					usage.InputTokens = payload.Message.Usage.InputTokens
				}
				if payload.Message.Usage.CacheReadInput != nil {
					usage.CacheReadTokens = payload.Message.Usage.CacheReadInput
				}
				if payload.Message.Usage.CacheCreationTokens != nil {
					usage.CacheCreationTokens = payload.Message.Usage.CacheCreationTokens
				}
			case "content_block_start":
				switch payload.ContentBlock.Type {
				case "thinking":
					thinking = ""
				case "tool_use":
					call := ToolCall{ID: payload.ContentBlock.ID, Name: payload.ContentBlock.Name}
					tools[payload.Index] = call
					return send(Event{Kind: ToolCallStart, Tool: &call})
				}
			case "content_block_delta":
				switch payload.Delta.Type {
				case "text_delta":
					return send(Event{Kind: TextDelta, Text: payload.Delta.Text})
				case "thinking_delta":
					thinking += payload.Delta.Thinking
					return send(Event{Kind: ThinkingDelta, Text: payload.Delta.Thinking})
				case "input_json_delta":
					call := tools[payload.Index]
					call.Arguments = append(call.Arguments, []byte(payload.Delta.PartialJSON)...)
					tools[payload.Index] = call
					return send(Event{Kind: ToolCallDelta, Text: payload.Delta.PartialJSON, Tool: &call})
				}
			case "content_block_stop":
				if call, ok := tools[payload.Index]; ok {
					if !json.Valid(call.Arguments) {
						return errors.New("invalid Anthropic tool arguments")
					}
					call.Complete = true
					tools[payload.Index] = call
					return send(Event{Kind: ToolCallComplete, Tool: &call})
				}
				if thinking != "" {
					text := thinking
					thinking = ""
					return send(Event{Kind: ThinkingComplete, Text: text})
				}
			case "message_delta":
				if payload.Usage.OutputTokens != nil {
					usage.OutputTokens = payload.Usage.OutputTokens
				}
				if payload.Delta.StopReason != "" {
					if err := send(Event{Kind: Usage, Usage: &usage}); err != nil {
						return err
					}
					return send(Event{Kind: StreamEnd, StopReason: payload.Delta.StopReason, Usage: &usage})
				}
			case "error":
				return &ProviderError{Class: ErrorProvider, Message: "Anthropic stream returned an error"}
			}
			return nil
		})
	})
}

func chooseModel(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
