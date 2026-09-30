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

func (p *HTTPProvider) Generate(ctx context.Context, prompt string, _ json.RawMessage) (ModelOutput, error) {
	out, err := p.GenerateStructured(ctx, prompt, SchemaNextAction)
	if err != nil {
		return ModelOutput{}, err
	}
	mo, err := extractJSON(out.Data, out.ProviderRequestID)
	return mo, err
}

// GenerateStructured sends one prompt constrained by the named schema and
// returns the assistant's raw JSON text without interpreting it.
func (p *HTTPProvider) GenerateStructured(ctx context.Context, prompt string, schema SchemaID) (StructuredOutput, error) {
	wire, name := wireSchema(schema)
	switch p.Config.Provider {
	case "openai":
		text, id, err := p.openai(ctx, prompt, wire, name)
		if err != nil {
			return StructuredOutput{}, err
		}
		return StructuredOutput{Data: json.RawMessage(text), ProviderRequestID: id}, nil
	case "anthropic":
		text, id, err := p.anthropic(ctx, prompt, wire, name)
		if err != nil {
			return StructuredOutput{}, err
		}
		return StructuredOutput{Data: json.RawMessage(text), ProviderRequestID: id}, nil
	case "gemini":
		text, id, err := p.gemini(ctx, prompt, wire, name)
		if err != nil {
			return StructuredOutput{}, err
		}
		return StructuredOutput{Data: json.RawMessage(text), ProviderRequestID: id}, nil
	case "openai-compatible":
		text, id, err := p.compatible(ctx, prompt, wire, name)
		if err != nil {
			return StructuredOutput{}, err
		}
		return StructuredOutput{Data: json.RawMessage(text), ProviderRequestID: id}, nil
	default:
		return StructuredOutput{}, APIError{Kind: "invalid_provider"}
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

func wireSchema(schema SchemaID) (map[string]any, string) {
	switch schema {
	case SchemaCriteriaProposal:
		return criteriaProposalSchema(), "criteria_proposal"
	default:
		return actionSchema(), "next_action"
	}
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
