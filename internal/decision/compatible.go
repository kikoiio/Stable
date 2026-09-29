package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) compatible(ctx context.Context, prompt string) (ModelOutput, error) {
	body := map[string]any{"model": p.Config.Model, "messages": []map[string]string{{"role": "system", "content": "Return exactly one JSON object matching the requested action schema. No markdown."}, {"role": "user", "content": prompt}}}
	data, id, err := p.post(ctx, endpoint(p.Config.BaseURL, "/chat/completions"), body, map[string]string{"Authorization": "Bearer " + p.Config.APIKey})
	if err != nil {
		return ModelOutput{}, err
	}
	var r struct {
		ID      string `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &r) != nil {
		return ModelOutput{}, APIError{Kind: "invalid_response"}
	}
	if len(r.Choices) != 1 || r.Choices[0].FinishReason != "stop" || r.Choices[0].Message.Role != "assistant" {
		return ModelOutput{}, APIError{Kind: "incomplete_response"}
	}
	if r.ID != "" {
		id = safeRequestID(r.ID)
	}
	return extractJSON([]byte(r.Choices[0].Message.Content), id)
}
