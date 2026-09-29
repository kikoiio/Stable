package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"proactive-agent/internal/artifact"
	"proactive-agent/internal/core"
	"proactive-agent/internal/policy"
	"proactive-agent/internal/store"
)

type lostReceiptBridge struct {
	store        *store.Store
	path         string
	executes     int
	preparedSeen bool
}

func (b *lostReceiptBridge) Call(ctx context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	result := core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID}
	if r.Kind == "inspect_design" {
		data, err := os.ReadFile(b.path)
		if err != nil {
			return result, err
		}
		result.Status = "observed"
		result.Postcondition = json.RawMessage(`{"connected":` + map[bool]string{true: "true", false: "false"}[string(data) == "fixed"] + `}`)
		return result, nil
	}
	snap, err := b.store.GetGoalSnapshot(ctx, "g")
	if err != nil {
		return result, err
	}
	b.preparedSeen = len(snap.Actions) == 1 && snap.Actions[0].Status == "prepared"
	b.executes++
	if err = os.WriteFile(b.path, []byte("fixed"), 0644); err != nil {
		return result, err
	}
	return result, ErrOutcomeUnknown
}

func TestPreparedBeforeEffectAndReconcileLostReceipt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "design.txt")
	if err := os.WriteFile(path, []byte("fault"), 0644); err != nil {
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
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.CreateGoal(ctx, core.Goal{ID: "g", Objective: "test", AllowedRoot: root, AllowedCapabilities: []string{"repair"}, CurrentArtifactID: initial})
	if err != nil {
		t.Fatal(err)
	}
	o := core.Observation{ID: "o", GoalID: "g", ArtifactID: initial, Facts: json.RawMessage(`{}`)}
	if err = s.RecordObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	d := core.Decision{ID: "d", AgentID: "agent-g", ObservationID: "o", Proposal: core.ProposedAction{Kind: "execute_capability", Capability: "repair", Target: path, Parameters: json.RawMessage(`{}`), ExpectedArtifactID: initial, Reason: "repair"}}
	if err = s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	bridge := &lostReceiptBridge{store: s, path: path}
	c := Coordinator{Store: s, Artifacts: art, Policy: policy.Policy{Declared: map[string]core.CapabilityDescriptor{"repair": {Name: "repair"}}}, Capabilities: map[string]Caller{"repair": bridge}, Postconditions: map[string]json.RawMessage{"repair": json.RawMessage(`{"connected":true}`)}}
	a, err := c.ExecuteOrReconcile(ctx, "d")
	if !errors.Is(err, ErrOutcomeUnknown) || a.Status != "outcome_unknown" {
		t.Fatalf("first action %+v %v", a, err)
	}
	if !bridge.preparedSeen {
		t.Fatal("effect occurred before prepared record")
	}
	a, err = c.ExecuteOrReconcile(ctx, "d")
	if err != nil || a.Status != "applied" {
		t.Fatalf("reconcile %+v %v", a, err)
	}
	if bridge.executes != 1 {
		t.Fatalf("executed %d times", bridge.executes)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || len(snap.Actions) != 1 {
		t.Fatalf("action records: %+v %v", snap.Actions, err)
	}
}
