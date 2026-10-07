package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"stable/internal/decision"
)

type ModelSelector struct{ model ChatModel }

func NewSelector(model ChatModel) *ModelSelector { return &ModelSelector{model: model} }

func (s *ModelSelector) Select(ctx context.Context, query string, candidates []MemoryHeader) ([]MemoryRef, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if s == nil || s.model == nil {
		return nil, errors.New("memory selector has no model")
	}
	input, err := json.Marshal(struct {
		Request    string         `json:"request"`
		Candidates []MemoryHeader `json:"candidates"`
	}{Request: query, Candidates: candidates})
	if err != nil {
		return nil, err
	}
	response, err := s.model.GenerateChat(ctx, []decision.ChatMessage{
		{Role: "system", Content: "Select at most five memory entries relevant to the request. The candidates contain metadata only. Return exactly JSON: {\"selected\":[{\"scope\":\"user|project\",\"filename\":\"...\"}]}. Only copy references from candidates. Do not invent references."},
		{Role: "user", Content: string(input)},
	})
	if err != nil {
		return nil, err
	}
	var output struct {
		Selected []MemoryRef `json:"selected"`
	}
	if err := decodeStrictJSON([]byte(response), &output); err != nil {
		return nil, fmt.Errorf("invalid selector JSON: %w", err)
	}
	allowed := make(map[MemoryRef]bool, len(candidates))
	for _, candidate := range candidates {
		allowed[MemoryRef{Scope: candidate.Scope, Filename: candidate.Filename}] = true
	}
	selected := make([]MemoryRef, 0, min(len(output.Selected), 5))
	seen := make(map[MemoryRef]bool)
	for _, ref := range output.Selected {
		if !allowed[ref] || seen[ref] {
			continue
		}
		seen[ref] = true
		selected = append(selected, ref)
		if len(selected) == 5 {
			break
		}
	}
	return selected, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
