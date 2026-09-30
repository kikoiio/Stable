package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) openai(ctx context.Context, prompt string, schema map[string]any, name string) (string, string, error) {
	body := map[string]any{"model": p.Config.Model, "input": prompt, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": name, "strict": true, "schema": schema}}}
	data, id, err := p.post(ctx, "https://api.openai.com/v1/responses", body, map[string]string{"Authorization": "Bearer " + p.Config.APIKey})
	if err != nil {
		return "", id, err
	}
	var r struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(data, &r) != nil {
		return "", id, APIError{Kind: "invalid_response"}
	}
	if r.Status != "completed" {
		return "", id, APIError{Kind: "incomplete_response"}
	}
	if r.ID != "" {
		id = safeRequestID(r.ID)
	}
	for _, o := range r.Output {
		for _, c := range o.Content {
			if c.Type == "refusal" {
				return "", id, APIError{Kind: "refusal"}
			}
			if c.Type == "output_text" {
				return c.Text, id, nil
			}
		}
	}
	return "", id, APIError{Kind: "empty_output"}
}
