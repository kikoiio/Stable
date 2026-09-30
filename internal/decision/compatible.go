package decision

import (
	"context"
	"encoding/json"
)

func (p *HTTPProvider) compatible(ctx context.Context, prompt string, schema map[string]any, name string) (string, string, error) {
	data, _ := json.Marshal(schema)
	system := compatibleSystemPrompt(name, string(data))
	body := map[string]any{"model": p.Config.Model, "response_format": map[string]string{"type": "json_object"}, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": prompt}}}
	resp, id, err := p.post(ctx, endpoint(p.Config.BaseURL, "/chat/completions"), body, map[string]string{"Authorization": "Bearer " + p.Config.APIKey})
	if err != nil {
		return "", id, err
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
	if json.Unmarshal(resp, &r) != nil {
		return "", id, APIError{Kind: "invalid_response"}
	}
	if len(r.Choices) != 1 || r.Choices[0].FinishReason != "stop" || r.Choices[0].Message.Role != "assistant" {
		return "", id, APIError{Kind: "incomplete_response"}
	}
	if r.ID != "" {
		id = safeRequestID(r.ID)
	}
	return r.Choices[0].Message.Content, id, nil
}

func compatibleSystemPrompt(name, schema string) string {
	if name == "criteria_proposal" {
		return "Return exactly one JSON object and nothing else (no markdown). It must have exactly these three keys and no others: " +
			"status, criteria, reason. status must be \"ok\" or \"reject\". criteria is an array of objects, each with exactly the keys " +
			"id (string), kind (string) and payload (object). Use an empty array when status is \"reject\". reason is a string written in the user's language. " +
			"Example: " +
			`{"status":"ok","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"reason":"mapped to ERC and connection checks"}` +
			". JSON schema: " + schema
	}
	return "Return exactly one JSON object and nothing else (no markdown). It must have exactly these six keys and no others: " +
		"kind, capability, target, parameters, expected_artifact_id, reason. All values are strings except parameters, which must be the empty object {}. " +
		"Use an empty string for a key that does not apply. Example: " +
		`{"kind":"observe","capability":"","target":"","parameters":{},"expected_artifact_id":"","reason":"check current state"}` +
		". JSON schema: " + schema
}
