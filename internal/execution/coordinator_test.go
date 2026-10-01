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
	rev := 0
	depRev := int64(0)
	d := core.Decision{ID: "d", AgentID: "agent-g", ObservationID: "o", Proposal: core.ProposedAction{Kind: "execute_capability", Capability: "repair", Target: path, Parameters: json.RawMessage(`{}`), ExpectedArtifactID: initial, Reason: "repair"}, CriteriaRevision: &rev, DependencyRevision: &depRev}
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

// countingBridge records capability calls and never changes the artifact.
type countingBridge struct{ calls int }

func (b *countingBridge) Call(_ context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	b.calls++
	return core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID, Status: "observed", Postcondition: json.RawMessage(`{"connected":false}`)}, nil
}

func staleGuardFixture(t *testing.T) (context.Context, *store.Store, *countingBridge, Coordinator, string, []byte) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "design.txt")
	if err := os.WriteFile(path, []byte("fault"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
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
	t.Cleanup(func() { s.Close() })
	if _, err = s.CreateGoal(ctx, core.Goal{ID: "g", Objective: "test", AllowedRoot: root, AllowedCapabilities: []string{"repair"}, CurrentArtifactID: initial}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordObservation(ctx, core.Observation{ID: "o", GoalID: "g", ArtifactID: initial, Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	bridge := &countingBridge{}
	c := Coordinator{Store: s, Artifacts: art, Policy: policy.Policy{Declared: map[string]core.CapabilityDescriptor{"repair": {Name: "repair"}}}, Capabilities: map[string]Caller{"repair": bridge}, Postconditions: map[string]json.RawMessage{"repair": json.RawMessage(`{"connected":true}`)}}
	return ctx, s, bridge, c, path, before
}

func recordDecision(t *testing.T, ctx context.Context, s *store.Store, id, path string, revision *int) {
	t.Helper()
	var dependencyRevision *int64
	if revision != nil {
		current := int64(0)
		dependencyRevision = &current
	}
	d := core.Decision{ID: id, AgentID: "agent-g", ObservationID: "o", Proposal: core.ProposedAction{Kind: "execute_capability", Capability: "repair", Target: path, Parameters: json.RawMessage(`{}`), ExpectedArtifactID: "unused", Reason: "repair"}, CriteriaRevision: revision, DependencyRevision: dependencyRevision}
	if err := s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
}

func bumpCriteria(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	payload := json.RawMessage(`{"max_violations":0}`)
	if _, err := s.UpdateGoalCriteria(ctx, "g", []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: payload}}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleDecisionCriteriaGuard(t *testing.T) {
	ctx, s, bridge, c, path, before := staleGuardFixture(t)

	// Legacy decision (nil revision): reservation refused, nothing written.
	recordDecision(t, ctx, s, "d-legacy", path, nil)
	if _, err := c.ExecuteOrReconcile(ctx, "d-legacy"); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("legacy decision: %v", err)
	}
	if bridge.calls != 0 {
		t.Fatalf("capability called %d times", bridge.calls)
	}

	// Current decision reserves and a prepared action appears.
	rev0 := 0
	recordDecision(t, ctx, s, "d-1", path, &rev0)
	if _, err := s.ReserveAction(ctx, core.ActionRecord{ID: "action-d-1", DecisionID: "d-1", ExpectedArtifactID: "x", DesiredPostcondition: json.RawMessage(`{"connected":true}`)}); err != nil {
		t.Fatal(err)
	}
	// Criteria change lands before execution: the prepared action is blocked
	// and the artifact stays byte-identical.
	bumpCriteria(t, ctx, s)
	a, err := c.ExecuteOrReconcile(ctx, "d-1")
	if err != nil || a.Status != "blocked" {
		t.Fatalf("stale prepared action %+v %v", a, err)
	}
	if bridge.calls != 0 {
		t.Fatalf("capability called %d times", bridge.calls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("artifact changed despite stale decision")
	}

	// A completed action of a stale decision stays as history, unchanged.
	if err = s.SetActionResult(ctx, "action-d-1", "applied", "sha", ""); err != nil {
		t.Fatal(err)
	}
	a, err = c.ExecuteOrReconcile(ctx, "d-1")
	if err != nil || a.Status != "applied" {
		t.Fatalf("completed action %+v %v", a, err)
	}
	if bridge.calls != 0 {
		t.Fatalf("capability called %d times", bridge.calls)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || len(snap.Actions) != 1 {
		t.Fatalf("actions %+v %v", snap.Actions, err)
	}
}
