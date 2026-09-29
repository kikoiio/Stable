package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) openai(ctx context.Context, prompt string, _ json.RawMessage) (ModelOutput, error) {
	body := map[string]any{"model": p.Config.Model, "input": prompt, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "next_action", "strict": true, "schema": actionSchema()}}}
	data, id, err := p.post(ctx, "https://api.openai.com/v1/responses", body, map[string]string{"Authorization": "Bearer " + p.Config.APIKey})
	if err != nil {
		return ModelOutput{}, err
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
		return ModelOutput{}, APIError{Kind: "invalid_response"}
	}
	if r.Status != "completed" {
		return ModelOutput{}, APIError{Kind: "incomplete_response"}
	}
	if r.ID != "" {
		id = safeRequestID(r.ID)
	}
	for _, o := range r.Output {
		for _, c := range o.Content {
			if c.Type == "refusal" {
				return ModelOutput{}, APIError{Kind: "refusal"}
			}
			if c.Type == "output_text" {
				return extractJSON([]byte(c.Text), id)
			}
		}
	}
	return ModelOutput{}, APIError{Kind: "empty_output"}
}
