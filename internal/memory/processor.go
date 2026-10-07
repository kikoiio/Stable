package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"stable/internal/decision"
)

const MaxMemoryChangesPerBatch = 20

type ModelProcessor struct{ model ChatModel }

func NewProcessor(model ChatModel) *ModelProcessor { return &ModelProcessor{model: model} }

func (p *ModelProcessor) Extract(ctx context.Context, input ExtractionInput) ([]MemoryChange, error) {
	if len(input.Messages) == 0 {
		return nil, nil
	}
	system := "Extract only durable user preferences, feedback, project context, and references. Return exactly JSON: {\"changes\":[{\"action\":\"upsert|delete\",\"scope\":\"user|project\",\"type\":\"user|feedback|project|reference\",\"name\":\"...\",\"description\":\"...\",\"body\":\"...\"}]}. Never output paths or frontmatter."
	if input.WorkKind == "goal" {
		system += " This is a goal work run. Extract only reusable user preferences, general project background, or references. Never store goal intent, goal text, acceptance criteria, progress, evidence, results, or conclusions, even if they appear in the supplied user messages."
	}
	request, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	changes, err := p.generate(ctx, system, request)
	if err != nil {
		return nil, err
	}
	return validateChanges(changes, input.WorkKind)
}

func (p *ModelProcessor) Consolidate(ctx context.Context, input ConsolidationInput) ([]MemoryChange, error) {
	if len(input.UserEntries) == 0 && len(input.ProjectEntries) == 0 {
		return nil, nil
	}
	request, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	changes, err := p.generate(ctx,
		"Consolidate the supplied memory entries by merging duplicates and removing stale entries when justified. Preserve scope and category boundaries. Return exactly JSON using changes with action upsert or delete, scope, type, name, description, and body. Never output paths or frontmatter.", request)
	if err != nil {
		return nil, err
	}
	return validateChanges(changes, "consolidation")
}

func (p *ModelProcessor) generate(ctx context.Context, system string, request []byte) ([]MemoryChange, error) {
	if p == nil || p.model == nil {
		return nil, errors.New("memory processor has no model")
	}
	response, err := p.model.GenerateChat(ctx, []decision.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: string(request)},
	})
	if err != nil {
		return nil, err
	}
	if len(response) > 2*MaxMemoryFileBytes {
		return nil, errors.New("memory model response exceeds 2 MiB limit")
	}
	var output struct {
		Changes []MemoryChange `json:"changes"`
	}
	if err := decodeStrictJSON([]byte(response), &output); err != nil {
		return nil, fmt.Errorf("invalid memory change JSON: %w", err)
	}
	return output.Changes, nil
}

func validateChanges(changes []MemoryChange, workKind string) ([]MemoryChange, error) {
	if len(changes) > MaxMemoryChangesPerBatch {
		return nil, fmt.Errorf("memory change batch exceeds %d entries", MaxMemoryChangesPerBatch)
	}
	validated := make([]MemoryChange, 0, len(changes))
	seen := make(map[string]bool)
	for _, change := range changes {
		wantScope, ok := ScopeForType(change.Type)
		if !ok || change.Scope != wantScope {
			return nil, errors.New("memory change type does not belong to requested scope")
		}
		if change.Action != ActionUpsert && change.Action != ActionDelete {
			return nil, errors.New("invalid memory change action")
		}
		change.Name = strings.TrimSpace(change.Name)
		change.Description = strings.TrimSpace(change.Description)
		change.Body = strings.TrimSpace(change.Body)
		if change.Name == "" || len(change.Name) > 160 || len(change.Description) > 1000 || len(change.Body) > MaxMemoryFileBytes-16*1024 {
			return nil, errors.New("invalid or oversized memory change")
		}
		if change.Action == ActionUpsert && change.Body == "" {
			return nil, errors.New("memory upsert requires a body")
		}
		key := string(change.Scope) + "\x00" + string(change.Type) + "\x00" + change.Name
		if seen[key] {
			return nil, errors.New("duplicate memory change in batch")
		}
		seen[key] = true
		validated = append(validated, change)
	}
	_ = workKind // policy text differs for goal extraction; types remain the allowlisted categories.
	return validated, nil
}
