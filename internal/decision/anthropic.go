package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) anthropic(ctx context.Context, prompt string, schema map[string]any, _ string) (string, string, error) {
	body := map[string]any{"model": p.Config.Model, "max_tokens": 1024, "messages": []map[string]string{{"role": "user", "content": prompt}}, "output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}}
	data, id, err := p.post(ctx, "https://api.anthropic.com/v1/messages", body, map[string]string{"x-api-key": p.Config.APIKey, "anthropic-version": "2023-06-01"})
	if err != nil {
		return "", id, err
	}
	var r struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &r) != nil {
		return "", id, APIError{Kind: "invalid_response"}
	}
	if r.StopReason != "end_turn" {
		return "", id, APIError{Kind: "incomplete_response"}
	}
	if r.ID != "" {
		id = safeRequestID(r.ID)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		return "", id, APIError{Kind: "empty_output"}
	}
	return r.Content[0].Text, id, nil
}
