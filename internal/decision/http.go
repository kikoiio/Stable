package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"stable/internal/appconfig"
	"stable/internal/core"
)

type HTTPProvider struct {
	Config appconfig.ModelConfig
	Client *http.Client
}

func (p *HTTPProvider) Descriptor() core.ModelDescriptor {
	host := map[string]string{"openai": "api.openai.com", "anthropic": "api.anthropic.com", "gemini": "generativelanguage.googleapis.com"}[p.Config.Provider]
	if p.Config.Provider == "openai-compatible" {
		u, _ := url.Parse(p.Config.BaseURL)
		if u != nil {
			host = u.Host
		}
	}
	return core.ModelDescriptor{Provider: p.Config.Provider, Model: p.Config.Model, Host: host}
}

func (p *HTTPProvider) Generate(ctx context.Context, prompt string, schema json.RawMessage) (ModelOutput, error) {
	switch p.Config.Provider {
	case "openai":
		return p.openai(ctx, prompt, schema)
	case "anthropic":
		return p.anthropic(ctx, prompt, schema)
	case "gemini":
		return p.gemini(ctx, prompt, schema)
	case "openai-compatible":
		return p.compatible(ctx, prompt)
	default:
		return ModelOutput{}, APIError{Kind: "invalid_provider"}
	}
}

func (p *HTTPProvider) post(ctx context.Context, endpoint string, body any, headers map[string]string) ([]byte, string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, "", APIError{Kind: "request_encoding"}
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, "", APIError{Kind: "invalid_endpoint"}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", APIError{Kind: "timeout"}
		}
		return nil, "", APIError{Kind: "network"}
	}
	defer resp.Body.Close()
	id := safeRequestID(resp.Header.Get("x-request-id"))
	if id == "" {
		id = safeRequestID(resp.Header.Get("request-id"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := "http_error"
		switch resp.StatusCode {
		case 401, 403:
			kind = "authentication"
		case 429:
			kind = "rate_limited"
		default:
			if resp.StatusCode >= 500 {
				kind = "provider_unavailable"
			}
		}
		return nil, id, APIError{Kind: kind}
	}
	limited := io.LimitReader(resp.Body, (2<<20)+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, id, APIError{Kind: "response_io"}
	}
	if len(out) > 2<<20 {
		return nil, id, APIError{Kind: "response_too_large"}
	}
	return out, id, nil
}

func actionSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "capability", "target", "parameters", "expected_artifact_id", "reason"}, "properties": map[string]any{
		"kind":       map[string]any{"type": "string", "enum": []string{"observe", "execute_capability", "open_computer", "wait", "ask_human"}},
		"capability": map[string]any{"type": "string"}, "target": map[string]any{"type": "string"},
		"parameters":           map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		"expected_artifact_id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"},
	}}
}

func extractJSON(data []byte, id string) (ModelOutput, error) {
	p, err := parseAction(data)
	if err != nil {
		return ModelOutput{}, err
	}
	return ModelOutput{Proposal: p, ProviderRequestID: id}, nil
}
func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }
