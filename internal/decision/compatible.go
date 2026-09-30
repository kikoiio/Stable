package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) compatible(ctx context.Context, prompt string) (ModelOutput, error) {
	schema, _ := json.Marshal(actionSchema())
	system := "Return exactly one JSON object and nothing else (no markdown). It must have exactly these six keys and no others: " +
		"kind, capability, target, parameters, expected_artifact_id, reason. All values are strings except parameters, which must be the empty object {}. " +
		"Use an empty string for a key that does not apply. Example: " +
		`{"kind":"observe","capability":"","target":"","parameters":{},"expected_artifact_id":"","reason":"check current state"}` +
		". JSON schema: " + string(schema)
	body := map[string]any{"model": p.Config.Model, "response_format": map[string]string{"type": "json_object"}, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": prompt}}}
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
