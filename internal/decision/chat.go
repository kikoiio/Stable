package decision

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatProvider interface {
	GenerateChat(context.Context, []ChatMessage) (string, error)
}

// GenerateChat requests ordinary text, without an action or criteria schema.
func (p *HTTPProvider) GenerateChat(ctx context.Context, messages []ChatMessage) (string, error) {
	var endpointURL string
	var body any
	var headers map[string]string
	switch p.Config.Provider {
	case "openai-compatible":
		endpointURL = endpoint(p.Config.BaseURL, "/chat/completions")
		body = map[string]any{"model": p.Config.Model, "messages": messages}
		headers = map[string]string{"Authorization": "Bearer " + p.Config.APIKey}
	case "openai":
		endpointURL = "https://api.openai.com/v1/responses"
		body = map[string]any{"model": p.Config.Model, "input": messages}
		headers = map[string]string{"Authorization": "Bearer " + p.Config.APIKey}
	case "anthropic":
		endpointURL = "https://api.anthropic.com/v1/messages"
		var turns []ChatMessage
		system := ""
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
			} else {
				turns = append(turns, m)
			}
		}
		body = map[string]any{"model": p.Config.Model, "max_tokens": 2048, "system": system, "messages": turns}
		headers = map[string]string{"x-api-key": p.Config.APIKey, "anthropic-version": "2023-06-01"}
	case "gemini":
		endpointURL = "https://generativelanguage.googleapis.com/v1beta/models/" + url.PathEscape(p.Config.Model) + ":generateContent"
		var contents []any
		system := ""
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
				continue
			}
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]string{"text": m.Content}}})
		}
		body = map[string]any{"systemInstruction": map[string]any{"parts": []any{map[string]string{"text": system}}}, "contents": contents}
		headers = map[string]string{"x-goog-api-key": p.Config.APIKey}
	default:
		return "", APIError{Kind: "invalid_provider"}
	}
	data, _, err := p.post(ctx, endpointURL, body, headers)
	if err != nil {
		return "", err
	}
	var result string
	switch p.Config.Provider {
	case "openai-compatible":
		var r struct {
			Choices []struct {
				FinishReason string      `json:"finish_reason"`
				Message      ChatMessage `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &r) != nil || len(r.Choices) != 1 || r.Choices[0].FinishReason != "stop" || r.Choices[0].Message.Role != "assistant" {
			return "", APIError{Kind: "incomplete_response"}
		}
		result = r.Choices[0].Message.Content
	case "openai":
		var r struct {
			Status string `json:"status"`
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(data, &r) != nil || r.Status != "completed" {
			return "", APIError{Kind: "incomplete_response"}
		}
		for _, output := range r.Output {
			for _, part := range output.Content {
				if part.Type == "output_text" {
					result += part.Text
				}
			}
		}
	case "anthropic":
		var r struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &r) != nil || r.StopReason != "end_turn" {
			return "", APIError{Kind: "incomplete_response"}
		}
		for _, part := range r.Content {
			if part.Type == "text" {
				result += part.Text
			}
		}
	case "gemini":
		var r struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
				Content      struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if json.Unmarshal(data, &r) != nil || len(r.Candidates) != 1 || r.Candidates[0].FinishReason != "STOP" {
			return "", APIError{Kind: "incomplete_response"}
		}
		for _, part := range r.Candidates[0].Content.Parts {
			result += part.Text
		}
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return "", APIError{Kind: "empty_output"}
	}
	return result, nil
}
