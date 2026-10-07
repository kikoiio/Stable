package memory

import (
	"context"
	"strings"
	"testing"
)

func TestProcessorGoalExtractionAppliesAllowlistedCategories(t *testing.T) {
	model := &recordingMemoryModel{response: `{"changes":[{"action":"upsert","scope":"user","type":"feedback","name":"Tone","description":"","body":"Be direct."}]}`}
	processor := NewProcessor(model)
	changes, err := processor.Extract(context.Background(), ExtractionInput{
		WorkKind: "goal",
		Messages: []ConversationText{{Kind: "goal_reply", Text: "Please keep responses direct."}},
	})
	if err != nil || len(changes) != 1 || changes[0].Body != "Be direct." {
		t.Fatalf("Extract() = %v, %v", changes, err)
	}
	if !strings.Contains(model.messages[0].Content, "Never store goal intent") || !strings.Contains(model.messages[1].Content, "keep responses direct") {
		t.Fatalf("goal extraction policy/input missing: %+v", model.messages)
	}
}

func TestProcessorRejectsInvalidBatches(t *testing.T) {
	bad := []string{
		`{"changes":[{"action":"upsert","scope":"project","type":"user","name":"bad","body":"x"}]}`,
		`{"changes":[{"action":"upsert","scope":"project","type":"project","name":"empty","body":""}]}`,
		`{"changes":[{"action":"upsert","scope":"project","type":"project","name":"x","body":"one"},{"action":"delete","scope":"project","type":"project","name":"x"}]}`,
	}
	for _, response := range bad {
		model := &recordingMemoryModel{response: response}
		if _, err := NewProcessor(model).Extract(context.Background(), ExtractionInput{Messages: []ConversationText{{Text: "context"}}}); err == nil {
			t.Errorf("invalid batch was accepted: %s", response)
		}
	}
	model := &recordingMemoryModel{response: `{"changes":[{"action":"upsert","scope":"user","type":"user","name":"x","body":"ok"}],"path":"/tmp/escape"}`}
	if _, err := NewProcessor(model).Extract(context.Background(), ExtractionInput{Messages: []ConversationText{{Text: "context"}}}); err == nil {
		t.Fatal("unknown output path field was accepted")
	}
}

func TestProcessorRejectsInvalidJSONAndCapsChanges(t *testing.T) {
	model := &recordingMemoryModel{response: "[]"}
	if _, err := NewProcessor(model).Extract(context.Background(), ExtractionInput{Messages: []ConversationText{{Text: "x"}}}); err == nil {
		t.Fatal("invalid response shape was accepted")
	}
	changes := make([]MemoryChange, MaxMemoryChangesPerBatch+1)
	for i := range changes {
		changes[i] = MemoryChange{Action: ActionDelete, Scope: ScopeProject, Type: TypeProject, Name: "entry" + string(rune('a'+i))}
	}
	if _, err := validateChanges(changes, "session"); err == nil {
		t.Fatal("oversized change batch was accepted")
	}
}
