package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/artifact"
	"stable/internal/core"
	"stable/internal/policy"
	"stable/internal/store"
)

type failingCandidateBridge struct {
	effectErr   error
	wrongDigest bool
}

func (b failingCandidateBridge) Call(_ context.Context, req core.CapabilityRequest) (core.CapabilityResult, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return core.CapabilityResult{}, err
	}
	if req.Kind == "inspect_design" {
		return core.CapabilityResult{ProtocolVersion: 1, OperationID: req.OperationID, Status: "observed", Postcondition: json.RawMessage(`{"connected":false}`)}, nil
	}
	if b.effectErr != nil {
		return core.CapabilityResult{}, b.effectErr
	}
	if err := os.WriteFile(payload.Path, []byte("fixed"), 0600); err != nil {
		return core.CapabilityResult{}, err
	}
	actual := ""
	if b.wrongDigest {
		actual = "not-the-candidate-digest"
	}
	return core.CapabilityResult{ProtocolVersion: 1, OperationID: req.OperationID, Status: "applied", ActualArtifactID: actual, Postcondition: json.RawMessage(`{"connected":true}`)}, nil
}

func coordinatorFailureFixture(t *testing.T, bridge Caller) (*store.Store, Coordinator, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "design.txt")
	if err := os.WriteFile(path, []byte("fault"), 0600); err != nil {
		t.Fatal(err)
	}
	art, err := artifact.New(root)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := art.Digest(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "failure-goal", Objective: "repair", AllowedRoot: root, AllowedCapabilities: []string{"repair"}, CurrentArtifactID: initial}); err != nil {
		t.Fatal(err)
	}
	snap, err := state.GetGoalSnapshot(ctx, "failure-goal")
	if err != nil {
		t.Fatal(err)
	}
	criteria, dependencies := snap.Goal.CriteriaRevision, snap.Goal.DependencyRevision
	if err = state.RecordObservation(ctx, core.Observation{ID: "failure-observation", GoalID: "failure-goal", ArtifactID: initial, Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = state.RecordDecision(ctx, core.Decision{ID: "failure-decision", AgentID: "agent-failure-goal", ObservationID: "failure-observation", CriteriaRevision: &criteria, DependencyRevision: &dependencies, Proposal: core.ProposedAction{Kind: "execute_capability", Capability: "repair", Target: path, ExpectedArtifactID: initial, Parameters: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	coordinator := Coordinator{Store: state, Artifacts: art, Policy: policy.Policy{Declared: map[string]core.CapabilityDescriptor{"repair": {Name: "repair"}}}, Capabilities: map[string]Caller{"repair": bridge}, Postconditions: map[string]json.RawMessage{"repair": json.RawMessage(`{"connected":true}`)}, Permissions: allowPermissionGate{}}
	return state, coordinator, path, initial
}

func TestCoordinatorBridgeFailureLeavesFormalProjectUntouched(t *testing.T) {
	state, coordinator, formalPath, initial := coordinatorFailureFixture(t, failingCandidateBridge{effectErr: errors.New("bridge startup failed")})
	action, err := coordinator.ExecuteOrReconcile(context.Background(), "failure-decision")
	if err == nil || action.Status != "outcome_unknown" {
		t.Fatalf("bridge failure action=%+v err=%v", action, err)
	}
	formal, readErr := os.ReadFile(formalPath)
	if readErr != nil || string(formal) != "fault" {
		t.Fatalf("formal project changed after bridge failure: %q %v", formal, readErr)
	}
	snapshot, err := state.GetGoalSnapshot(context.Background(), "failure-goal")
	if err != nil || len(snapshot.Actions) != 1 || snapshot.Actions[0].ExpectedArtifactID != initial {
		t.Fatalf("failure action was not durably recorded: %+v %v", snapshot.Actions, err)
	}
}

func TestCoordinatorRejectsCapabilityDigestMismatchWithoutFormalWrite(t *testing.T) {
	_, coordinator, formalPath, _ := coordinatorFailureFixture(t, failingCandidateBridge{wrongDigest: true})
	action, err := coordinator.ExecuteOrReconcile(context.Background(), "failure-decision")
	if err == nil || action.Status != "outcome_unknown" {
		t.Fatalf("digest mismatch action=%+v err=%v", action, err)
	}
	formal, readErr := os.ReadFile(formalPath)
	if readErr != nil || string(formal) != "fault" {
		t.Fatalf("formal project changed after digest mismatch: %q %v", formal, readErr)
	}
}
