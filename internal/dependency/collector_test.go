package dependency

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/core"
)

type collectorCaller func(context.Context, core.CapabilityRequest) (core.CapabilityResult, error)

func (f collectorCaller) Call(ctx context.Context, req core.CapabilityRequest) (core.CapabilityResult, error) {
	return f(ctx, req)
}

func TestKiCadCollector(t *testing.T) {
	dependencies := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.8", Fingerprint: "erc", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection", Available: true},
	}
	body, err := json.Marshal(map[string]any{"dependencies": dependencies})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	allowed := filepath.Join(root, "goal-1")
	if err := os.MkdirAll(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	goal := core.Goal{ID: "goal-1", ArtifactPath: filepath.Join(allowed, "sensor.kicad_sch"), AllowedRoot: allowed, CurrentArtifactID: "artifact"}
	calls := 0
	collector := KiCadCollector{Kicad: collectorCaller(func(_ context.Context, req core.CapabilityRequest) (core.CapabilityResult, error) {
		calls++
		if req.Kind != "kicad.describe_dependencies" || req.GoalID != goal.ID || req.ExpectedArtifactID != goal.CurrentArtifactID {
			t.Fatalf("unexpected capability request: %+v", req)
		}
		var payload map[string]string
		if err := json.Unmarshal(req.Payload, &payload); err != nil || payload["path"] != goal.ArtifactPath || payload["allowed_root"] != goal.AllowedRoot {
			t.Fatalf("dependency path payload: %v %v", payload, err)
		}
		if payload["project_root"] != goal.AllowedRoot {
			t.Fatalf("dependency collection must be scoped to the formal project: %v", payload)
		}
		for _, key := range []string{"candidate_root", "run_root"} {
			scratch := payload[key]
			if scratch == "" || !strings.HasPrefix(scratch, root+string(os.PathSeparator)) {
				t.Fatalf("dependency scratch %s outside the collector run root: %v", key, payload)
			}
		}
		return core.CapabilityResult{Status: "observed", ActualArtifactID: "artifact", Postcondition: body}, nil
	})}
	got, err := collector.Collect(context.Background(), goal)
	if err != nil || len(got) != 2 || calls != 1 || got[0].Fingerprint != "erc" {
		t.Fatalf("collect: %+v calls=%d err=%v", got, calls, err)
	}
}

func TestKiCadCollectorRejectsUnexpectedResults(t *testing.T) {
	allowed := filepath.Join(t.TempDir(), "g")
	if err := os.MkdirAll(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	goal := core.Goal{ID: "g", ArtifactPath: filepath.Join(allowed, "design.kicad_sch"), AllowedRoot: allowed, CurrentArtifactID: "artifact"}
	for name, response := range map[string]core.CapabilityResult{
		"blocked":                 {Status: "blocked"},
		"mid-call design change":  {Status: "stale", ActualArtifactID: "other", Postcondition: json.RawMessage(`{"dependencies":[]}`)},
		"malformed postcondition": {Status: "observed", ActualArtifactID: "artifact", Postcondition: json.RawMessage(`{bad`)},
		"missing family":          {Status: "observed", ActualArtifactID: "artifact", Postcondition: json.RawMessage(`{"dependencies":[]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			collector := KiCadCollector{Kicad: collectorCaller(func(context.Context, core.CapabilityRequest) (core.CapabilityResult, error) { return response, nil })}
			if _, err := collector.Collect(context.Background(), goal); err == nil {
				t.Fatalf("invalid collector result accepted: %v", err)
			}
		})
	}
}

func TestKiCadCollectorAcceptsStableArtifactNewerThanRecordedGoal(t *testing.T) {
	allowed := filepath.Join(t.TempDir(), "g")
	if err := os.MkdirAll(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	goal := core.Goal{ID: "g", ArtifactPath: filepath.Join(allowed, "design.kicad_sch"), AllowedRoot: allowed, CurrentArtifactID: "recorded"}
	dependencies := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.8", Fingerprint: "erc-v1", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	body, _ := json.Marshal(map[string]any{"dependencies": dependencies})
	collector := KiCadCollector{Kicad: collectorCaller(func(context.Context, core.CapabilityRequest) (core.CapabilityResult, error) {
		return core.CapabilityResult{Status: "observed", ActualArtifactID: "stable-new-design", Postcondition: body}, nil
	})}
	got, err := collector.Collect(context.Background(), goal)
	if err != nil || len(got) != 2 {
		t.Fatalf("stable current design should yield dependency snapshot before the observer records its new digest: %+v err=%v", got, err)
	}
}
