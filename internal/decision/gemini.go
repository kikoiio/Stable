package decision

import (
	"context"
	"encoding/json"
	"net/url"
)

func (p *HTTPProvider) gemini(ctx context.Context, prompt string, _ json.RawMessage) (ModelOutput, error) {
	body := map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": prompt}}}}, "generationConfig": map[string]any{"responseMimeType": "application/json", "responseJsonSchema": actionSchema()}}
	path := "https://generativelanguage.googleapis.com/v1beta/models/" + url.PathEscape(p.Config.Model) + ":generateContent"
	data, id, err := p.post(ctx, path, body, map[string]string{"x-goog-api-key": p.Config.APIKey})
	if err != nil {
		return ModelOutput{}, err
	}
	var r struct {
		ResponseID string `json:"responseId"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &r) != nil {
		return ModelOutput{}, APIError{Kind: "invalid_response"}
	}
	if len(r.Candidates) != 1 || r.Candidates[0].FinishReason != "STOP" {
		return ModelOutput{}, APIError{Kind: "incomplete_response"}
	}
	if r.ResponseID != "" {
		id = safeRequestID(r.ResponseID)
	}
	if len(r.Candidates[0].Content.Parts) != 1 {
		return ModelOutput{}, APIError{Kind: "empty_output"}
	}
	return extractJSON([]byte(r.Candidates[0].Content.Parts[0].Text), id)
}
