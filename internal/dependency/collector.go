package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"stable/internal/core"
)

// CapabilityCaller is the subset of the KiCad bridge used to describe check
// dependencies without running or changing a check.
type CapabilityCaller interface {
	Call(context.Context, core.CapabilityRequest) (core.CapabilityResult, error)
}

type KiCadCollector struct {
	Kicad CapabilityCaller
}

func (c KiCadCollector) Collect(ctx context.Context, goal core.Goal) ([]core.DependencySnapshot, error) {
	if c.Kicad == nil {
		return nil, errors.New("KiCad dependency bridge is unavailable")
	}
	if goal.ArtifactPath == "" || goal.AllowedRoot == "" {
		return nil, errors.New("goal dependency paths are missing")
	}
	requestID := fmt.Sprintf("dependencies-%d", time.Now().UTC().UnixNano())
	payload, err := json.Marshal(map[string]string{"path": goal.ArtifactPath, "allowed_root": goal.AllowedRoot})
	if err != nil {
		return nil, err
	}
	result, err := c.Kicad.Call(ctx, core.CapabilityRequest{
		ProtocolVersion:    1,
		OperationID:        requestID,
		Kind:               "kicad.describe_dependencies",
		GoalID:             goal.ID,
		ExpectedArtifactID: goal.CurrentArtifactID,
		Payload:            payload,
	})
	if err != nil {
		return nil, fmt.Errorf("collect KiCad dependencies: %w", err)
	}
	if result.Status != "observed" {
		return nil, fmt.Errorf("dependency collector returned %q: %s", result.Status, result.ErrorCode)
	}
	var output struct {
		Dependencies []core.DependencySnapshot `json:"dependencies"`
	}
	if err = json.Unmarshal(result.Postcondition, &output); err != nil {
		return nil, fmt.Errorf("decode dependency snapshots: %w", err)
	}
	if len(output.Dependencies) != 2 {
		return nil, fmt.Errorf("dependency collector returned %d families, expected two", len(output.Dependencies))
	}
	return output.Dependencies, nil
}
