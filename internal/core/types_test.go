package core

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestValidateCriterionERCClean(t *testing.T) {
	if err := ValidateCriterion(Criterion{ID: "erc", Kind: CriterionKindERCClean, Payload: mustJSON(t, map[string]any{"max_violations": 0})}); err != nil {
		t.Fatalf("valid erc_clean rejected: %v", err)
	}
	if err := ValidateCriterion(Criterion{ID: "erc", Kind: CriterionKindERCClean, Payload: mustJSON(t, map[string]any{"max_violations": -1})}); err == nil {
		t.Fatal("negative max_violations accepted")
	}
	if err := ValidateCriterion(Criterion{ID: "erc", Kind: CriterionKindERCClean, Payload: mustJSON(t, map[string]any{"max_violations": 0, "extra": 1})}); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := ValidateCriterion(Criterion{ID: "erc", Kind: CriterionKindERCClean}); err == nil {
		t.Fatal("missing payload accepted")
	}
}

func TestValidateCriterionConnectionPresent(t *testing.T) {
	valid := Criterion{ID: "conn", Kind: CriterionKindConnectionPresent, Payload: mustJSON(t, map[string]any{"endpoint_a": SensorEndpointA, "endpoint_b": SensorEndpointB})}
	if err := ValidateCriterion(valid); err != nil {
		t.Fatalf("valid connection_present rejected: %v", err)
	}
	bad := Criterion{ID: "conn", Kind: CriterionKindConnectionPresent, Payload: mustJSON(t, map[string]any{"endpoint_a": "R1.1", "endpoint_b": "C3.2"})}
	if err := ValidateCriterion(bad); err == nil {
		t.Fatal("unsupported endpoints accepted")
	}
}

func TestValidateCriteriaVocabularyAndIDs(t *testing.T) {
	if err := ValidateCriteria(nil); err != nil {
		t.Fatalf("empty criteria rejected: %v", err)
	}
	if err := ValidateCriteria([]Criterion{{ID: "x", Kind: "kicad.beautiful"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	dup := []Criterion{
		{ID: "erc", Kind: CriterionKindERCClean, Payload: mustJSON(t, map[string]any{"max_violations": 0})},
		{ID: "erc", Kind: CriterionKindERCClean, Payload: mustJSON(t, map[string]any{"max_violations": 0})},
	}
	if err := ValidateCriteria(dup); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
}
