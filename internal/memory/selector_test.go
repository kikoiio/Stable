package memory

import (
	"context"
	"strings"
	"testing"

	"stable/internal/decision"
)

type recordingMemoryModel struct {
	response string
	err      error
	messages []decision.ChatMessage
}

func (m *recordingMemoryModel) GenerateChat(_ context.Context, messages []decision.ChatMessage) (string, error) {
	m.messages = append([]decision.ChatMessage(nil), messages...)
	return m.response, m.err
}

func TestSelectorUsesMetadataOnlyAndBoundsReferences(t *testing.T) {
	candidates := make([]MemoryHeader, 7)
	for i := range candidates {
		candidates[i] = MemoryHeader{Scope: ScopeProject, Type: TypeProject, Filename: "entry-" + string(rune('a'+i)) + ".md", Name: "Entry", Description: "description"}
	}
	model := &recordingMemoryModel{response: `{"selected":[{"scope":"project","filename":"entry-a.md"},{"scope":"project","filename":"entry-a.md"},{"scope":"user","filename":"invented.md"},{"scope":"project","filename":"entry-b.md"},{"scope":"project","filename":"entry-c.md"},{"scope":"project","filename":"entry-d.md"},{"scope":"project","filename":"entry-e.md"},{"scope":"project","filename":"entry-f.md"}]}`}
	selected, err := NewSelector(model).Select(context.Background(), "request text", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 5 {
		t.Fatalf("got %d selected refs, want at most 5: %v", len(selected), selected)
	}
	input := model.messages[1].Content
	if !strings.Contains(input, "request text") || !strings.Contains(input, "entry-a.md") || strings.Contains(input, "secret body") {
		t.Fatalf("unexpected selector input: %s", input)
	}
}

func TestSelectorSkipsUnknownReferencesAndRejectsMalformedJSON(t *testing.T) {
	candidates := []MemoryHeader{{Scope: ScopeUser, Type: TypeUser, Filename: "style.md"}}
	model := &recordingMemoryModel{response: `{"selected":[{"scope":"project","filename":"foreign.md"},{"scope":"user","filename":"style.md"}]}`}
	selected, err := NewSelector(model).Select(context.Background(), "query", candidates)
	if err != nil || len(selected) != 1 || selected[0].Filename != "style.md" {
		t.Fatalf("selected=%v, err=%v", selected, err)
	}
	model.response = "not json"
	if _, err := NewSelector(model).Select(context.Background(), "query", candidates); err == nil {
		t.Fatal("malformed selector JSON was accepted")
	}
}
