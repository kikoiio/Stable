package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"stable/internal/appconfig"
	"stable/internal/core"
)

type ModelOutput = core.ModelDecisionOutput
type ModelProvider interface {
	Descriptor() core.ModelDescriptor
	Generate(context.Context, string, json.RawMessage) (ModelOutput, error)
}

type APIError struct{ Kind string }

func (e APIError) Error() string     { return "model request failed: " + e.Kind }
func (e APIError) ErrorKind() string { return e.Kind }
func ErrorKind(err error) string {
	var e APIError
	if errors.As(err, &e) {
		return e.Kind
	}
	return "model_error"
}

type ProviderDecider struct {
	Provider ModelProvider
	Schema   json.RawMessage
}

func (d ProviderDecider) Descriptor() core.ModelDescriptor { return d.Provider.Descriptor() }
func (d ProviderDecider) Decide(ctx context.Context, in core.DecisionContext) (core.ProposedAction, error) {
	o, e := d.DecideModel(ctx, in)
	return o.Proposal, e
}
func (d ProviderDecider) DecideModel(ctx context.Context, in core.DecisionContext) (ModelOutput, error) {
	prompt, err := makePrompt(in)
	if err != nil {
		return ModelOutput{}, err
	}
	return d.Provider.Generate(ctx, prompt, d.Schema)
}

func NewProvider(c appconfig.ModelConfig) (ModelProvider, error) {
	if c.APIKey == "" {
		return nil, APIError{Kind: "missing_key"}
	}
	switch c.Provider {
	case "openai", "anthropic", "gemini", "openai-compatible":
	default:
		return nil, APIError{Kind: "invalid_provider"}
	}
	return &HTTPProvider{Config: c}, nil
}

func parseAction(data []byte) (core.ProposedAction, error) {
	var p core.ProposedAction
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return p, APIError{Kind: "invalid_json"}
	}
	for _, key := range []string{"kind", "capability", "target", "parameters", "expected_artifact_id", "reason"} {
		if _, ok := fields[key]; !ok {
			return p, APIError{Kind: "invalid_action"}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, APIError{Kind: "invalid_action"}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return p, APIError{Kind: "invalid_json"}
	}
	if err := Validate(p); err != nil {
		return p, APIError{Kind: "invalid_action"}
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(p.Parameters, &params) != nil || params == nil || len(params) != 0 {
		return p, APIError{Kind: "invalid_action"}
	}
	return p, nil
}

func safeRequestID(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	for _, r := range s {
		if r < 32 || r > 126 {
			return ""
		}
	}
	return strings.TrimSpace(s)
}

func firstText(parts []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) (string, error) {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "output_text" {
			b.WriteString(p.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w", APIError{Kind: "empty_output"})
	}
	return b.String(), nil
}
