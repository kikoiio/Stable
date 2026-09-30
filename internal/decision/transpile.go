package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"stable/internal/core"
)

// criteriaProposalSchema is the wire schema for natural-language criteria
// transpilation; schemas/criteria_proposal.schema.json must stay identical
// (asserted by TestCriteriaProposalSchemaFileMatches).
func criteriaProposalSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"status", "criteria", "reason"}, "properties": map[string]any{
		"status": map[string]any{"type": "string", "enum": []string{"ok", "reject"}},
		"criteria": map[string]any{"type": "array", "items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"id", "kind", "payload"},
			"properties": map[string]any{
				"id":      map[string]any{"type": "string"},
				"kind":    map[string]any{"type": "string"},
				"payload": map[string]any{"type": "object", "additionalProperties": true},
			},
		}},
		"reason": map[string]any{"type": "string"},
	}}
}

type TranspileResult struct {
	Status            string // core.ProposalPending criteria when "ok"; reason when "reject"
	Criteria          []core.Criterion
	Reason            string
	ProviderRequestID string
}

func transpilePrompt(nl string) string {
	return "Translate the user's acceptance requirements for a KiCad sensor-board repair goal into machine-verifiable criteria. " +
		"Only this vocabulary exists: " +
		`kicad.erc_clean with payload {"max_violations": <integer >= 0>}; ` +
		`sensor.connection_present with payload {"endpoint_a": "RT1.2", "endpoint_b": "J1.2"} (exactly these endpoints, nothing else). ` +
		"If any requirement cannot be expressed with these kinds, set status to reject and explain why in the user's language; never invent kinds, fields or endpoints. " +
		"Give each criterion a short kebab-case id. User requirements: " + nl
}

// Transpile converts a natural-language acceptance description into criteria
// via the model, or rejects it. Anything outside the vocabulary is rejected
// here before it can reach a goal.
func Transpile(ctx context.Context, p StructuredProvider, nl string) (TranspileResult, error) {
	out, err := p.GenerateStructured(ctx, transpilePrompt(nl), SchemaCriteriaProposal)
	if err != nil {
		return TranspileResult{}, err
	}
	res, err := parseTranspile(out.Data)
	if err != nil {
		return TranspileResult{}, err
	}
	res.ProviderRequestID = out.ProviderRequestID
	return res, nil
}

func parseTranspile(data []byte) (TranspileResult, error) {
	var wire struct {
		Status   string          `json:"status"`
		Criteria []wireCriterion `json:"criteria"`
		Reason   string          `json:"reason"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return TranspileResult{}, APIError{Kind: "invalid_transpile"}
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return TranspileResult{}, APIError{Kind: "invalid_transpile"}
	}
	switch wire.Status {
	case "reject":
		if wire.Reason == "" {
			return TranspileResult{}, APIError{Kind: "invalid_transpile"}
		}
		return TranspileResult{Status: "reject", Reason: wire.Reason}, nil
	case "ok":
	default:
		return TranspileResult{}, APIError{Kind: "invalid_transpile"}
	}
	if len(wire.Criteria) == 0 {
		return TranspileResult{}, APIError{Kind: "invalid_transpile"}
	}
	criteria := make([]core.Criterion, 0, len(wire.Criteria))
	for _, wc := range wire.Criteria {
		payload, err := wc.payloadBytes()
		if err != nil {
			return TranspileResult{}, APIError{Kind: "invalid_transpile"}
		}
		criteria = append(criteria, core.Criterion{ID: wc.ID, Kind: wc.Kind, Payload: payload})
	}
	if err := core.ValidateCriteria(criteria); err != nil {
		return TranspileResult{}, APIError{Kind: "out_of_vocabulary"}
	}
	return TranspileResult{Status: "ok", Criteria: criteria, Reason: wire.Reason}, nil
}

type wireCriterion struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func (w wireCriterion) payloadBytes() (json.RawMessage, error) {
	if len(w.Payload) == 0 {
		return nil, fmt.Errorf("missing payload")
	}
	var v map[string]any
	if err := json.Unmarshal(w.Payload, &v); err != nil || v == nil {
		return nil, fmt.Errorf("payload must be an object")
	}
	return w.Payload, nil
}
