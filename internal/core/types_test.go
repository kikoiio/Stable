package core_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"stable/internal/core"
)

func TestDependencyTypes(t *testing.T) {
	revision := int64(7)
	snapshot := core.DependencySnapshot{
		SchemaVersion:  1,
		Family:         core.CheckFamilyERC,
		Sources:        []core.DependencySource{{Kind: "project_erc", Identity: "erc", Digest: "sha256:abc", State: "present"}},
		CheckerID:      "kicad-cli",
		CheckerVersion: "9.0.8",
		Fingerprint:    "sha256:def",
		Available:      true,
	}
	goal := core.Goal{ID: "g1", DependencyRevision: revision}
	decision := core.Decision{ID: "d1", Proposal: core.ProposedAction{Parameters: json.RawMessage(`{}`)}, DependencyRevision: &revision}
	provenance := core.EvidenceProvenance{SchemaVersion: 2, Family: core.CheckFamilyERC, Dependency: &snapshot}
	token := core.VerificationToken{GoalID: "g1", DependencyRevision: revision}
	refresh := core.DependencyRefresh{ChangedFamilies: []core.CheckFamily{core.CheckFamilyERC}, DependencyRevision: revision, Snapshot: core.GoalSnapshot{Goal: goal, Dependencies: []core.DependencySnapshot{snapshot}}}

	for name, value := range map[string]any{
		"goal": goal, "decision": decision, "provenance": provenance, "token": token, "refresh": refresh,
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			switch value := value.(type) {
			case core.Goal:
				var got core.Goal
				if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, value) {
					t.Fatalf("goal round trip: got %#v, err %v", got, err)
				}
			case core.Decision:
				var got core.Decision
				if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, value) {
					t.Fatalf("decision round trip: got %#v, err %v", got, err)
				}
			case core.EvidenceProvenance:
				var got core.EvidenceProvenance
				if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, value) {
					t.Fatalf("provenance round trip: got %#v, err %v", got, err)
				}
			case core.VerificationToken:
				var got core.VerificationToken
				if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, value) {
					t.Fatalf("token round trip: got %#v, err %v", got, err)
				}
			case core.DependencyRefresh:
				var got core.DependencyRefresh
				if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, value) {
					t.Fatalf("refresh round trip: got %#v, err %v", got, err)
				}
			}
		})
	}

	var legacy core.Goal
	if err := json.Unmarshal([]byte(`{"id":"legacy","criteria_revision":3,"status":"verified"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ID != "legacy" || legacy.CriteriaRevision != 3 || legacy.DependencyRevision != 0 || legacy.Status != core.GoalVerified {
		t.Fatalf("legacy goal did not retain old fields with unknown dependency revision: %#v", legacy)
	}
	var legacyProvenance core.EvidenceProvenance
	if err := json.Unmarshal([]byte(`{"schema_version":1,"claim":"old","coverage":"all"}`), &legacyProvenance); err != nil {
		t.Fatal(err)
	}
	if legacyProvenance.SchemaVersion != 1 || legacyProvenance.Dependency != nil || legacyProvenance.Family != "" {
		t.Fatalf("legacy provenance was not preserved as v1/unknown: %#v", legacyProvenance)
	}
}

func TestEvidenceCurrentDependencies(t *testing.T) {
	revision := 4
	current := core.DependencySnapshot{
		SchemaVersion:  1,
		Family:         core.CheckFamilyERC,
		Sources:        []core.DependencySource{{Kind: "project_erc", Identity: "settings", Digest: "sha256:abc", State: "present"}},
		CheckerID:      "kicad-cli-erc",
		CheckerVersion: "9.0.8",
		Fingerprint:    "sha256:all-inputs",
		Available:      true,
	}
	makeEvidence := func() core.Evidence {
		frozen := current
		frozen.Sources = append([]core.DependencySource(nil), current.Sources...)
		return core.Evidence{
			ID: "erc-1", ArtifactID: "artifact-1", Result: "pass", CriteriaRevision: &revision,
			Provenance: &core.EvidenceProvenance{
				SchemaVersion: 2, Claim: "ERC within threshold", Coverage: "all ERC criteria",
				CheckerID: current.CheckerID, CheckerVersion: current.CheckerVersion,
				SourceLevel: "tool_check", InvalidationRule: "artifact, criteria or dependency change",
				Family: current.Family, Dependency: &frozen,
			},
		}
	}
	goal := core.Goal{ID: "g", CriteriaRevision: revision, CurrentArtifactID: "artifact-1"}
	if !core.EvidenceCurrentWithDependencies(goal, "artifact-1", []core.DependencySnapshot{current}, makeEvidence()) {
		t.Fatal("matching available v2 evidence should be current")
	}

	tests := map[string]func(*core.Evidence, *[]core.DependencySnapshot){
		"changed fingerprint": func(_ *core.Evidence, snapshots *[]core.DependencySnapshot) {
			(*snapshots)[0].Fingerprint = "sha256:changed"
		},
		"unavailable current input": func(_ *core.Evidence, snapshots *[]core.DependencySnapshot) { (*snapshots)[0].Available = false },
		"checker version changed":   func(_ *core.Evidence, snapshots *[]core.DependencySnapshot) { (*snapshots)[0].CheckerVersion = "9.1.0" },
		"family absent": func(_ *core.Evidence, snapshots *[]core.DependencySnapshot) {
			*snapshots = []core.DependencySnapshot{{Family: core.CheckFamilyConnection, Available: true}}
		},
		"duplicate family snapshot": func(_ *core.Evidence, snapshots *[]core.DependencySnapshot) {
			*snapshots = append(*snapshots, (*snapshots)[0])
		},
		"legacy v1 provenance":          func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.Provenance.SchemaVersion = 1 },
		"screenshot source":             func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.Provenance.SourceLevel = "observation" },
		"mismatched family":             func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.Provenance.Family = core.CheckFamilyConnection },
		"missing frozen dependency":     func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.Provenance.Dependency = nil },
		"frozen dependency unavailable": func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.Provenance.Dependency.Available = false },
		"criteria revision stale":       func(e *core.Evidence, _ *[]core.DependencySnapshot) { stale := 3; e.CriteriaRevision = &stale },
		"artifact stale":                func(e *core.Evidence, _ *[]core.DependencySnapshot) { e.ArtifactID = "old-artifact" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			evidence := makeEvidence()
			snapshots := []core.DependencySnapshot{current}
			mutate(&evidence, &snapshots)
			if core.EvidenceCurrentWithDependencies(goal, "artifact-1", snapshots, evidence) {
				t.Fatal("invalid or stale evidence was accepted as current")
			}
		})
	}
}
