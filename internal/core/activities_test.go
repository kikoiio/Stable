package core_test

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

type callerFunc func(context.Context, core.CapabilityRequest) (core.CapabilityResult, error)

func (f callerFunc) Call(ctx context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
	return f(ctx, r)
}

type deciderFunc func(context.Context, core.DecisionContext) (core.ProposedAction, error)

func (f deciderFunc) Decide(ctx context.Context, r core.DecisionContext) (core.ProposedAction, error) {
	return f(ctx, r)
}

type executorFunc func(context.Context, string) (core.ActionRecord, error)

func (f executorFunc) ExecuteOrReconcile(ctx context.Context, id string) (core.ActionRecord, error) {
	return f(ctx, id)
}

func fixtureActivities(t *testing.T) (*core.Activities, *store.Store, string) {
	t.Helper()
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
	hash, err := art.Digest(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, err = s.CreateGoal(ctx, core.Goal{ID: "g", Objective: "test", AllowedRoot: root, ArtifactPath: path, CurrentArtifactID: hash, AllowedCapabilities: []string{"repair"}, Criteria: []core.Criterion{{ID: "erc", Kind: "kicad.erc_clean", Payload: json.RawMessage(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	kicad := callerFunc(func(_ context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
		current, _ := art.Digest(ctx, path)
		result := core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID, ActualArtifactID: current}
		switch r.Kind {
		case "inspect_design":
			result.Status = "observed"
			result.Postcondition = json.RawMessage(`{"sensor.supported":true,"sensor.connection_present":false}`)
		case "kicad.run_erc":
			var payload map[string]string
			_ = json.Unmarshal(r.Payload, &payload)
			_ = os.WriteFile(payload["report_path"], []byte(`{"sheets":[]}`), 0644)
			result.Status = "pass"
			result.EvidencePaths = []string{payload["report_path"]}
			result.Postcondition = json.RawMessage(`{"violation_count":0}`)
		default:
			return result, errors.New("unknown capability")
		}
		return result, nil
	})
	a := &core.Activities{State: s, Artifacts: art, Kicad: kicad, Policy: policy.Policy{}}
	return a, s, path
}

func TestObserveInvalidatesOldEvidenceAndVerifyCurrentDigest(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	first, err := a.ObserveGoal(ctx, "g", "timer-1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordEvidence(ctx, core.Evidence{ID: "old", GoalID: "g", CriterionID: "erc", ArtifactID: first.ArtifactID, Kind: "kicad.erc", Result: "pass", ReportPath: "old.json"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = a.VerifyGoal(ctx, "g"); err == nil {
		t.Fatal("verified stale design")
	}
	second, err := a.ObserveGoal(ctx, "g", "timer-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.ArtifactID == first.ArtifactID {
		t.Fatal("digest did not change")
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || snap.Evidence[0].Result != "stale" {
		t.Fatalf("evidence %+v %v", snap.Evidence, err)
	}
	e, err := a.VerifyGoal(ctx, "g")
	if err != nil || e.Result != "pass" {
		t.Fatalf("verification %+v %v", e, err)
	}
	snap, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil || snap.Goal.Status != core.GoalVerified {
		t.Fatalf("goal %+v %v", snap.Goal, err)
	}
}

func TestEvaluateRejectsUnauthorizedDecision(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	executed := false
	a.Decider = deciderFunc(func(_ context.Context, d core.DecisionContext) (core.ProposedAction, error) {
		return core.ProposedAction{Kind: "execute_capability", Capability: "delete", Target: path, Parameters: json.RawMessage(`{}`), ExpectedArtifactID: d.Observation.ArtifactID, Reason: "bad proposal"}, nil
	})
	a.Executor = executorFunc(func(context.Context, string) (core.ActionRecord, error) {
		executed = true
		return core.ActionRecord{}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "timer-0")
	if err != nil || done || executed {
		t.Fatalf("result done=%v executed=%v err=%v", done, executed, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || snap.Goal.Status != core.GoalNeedsHuman || len(snap.Decisions) != 1 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
}
