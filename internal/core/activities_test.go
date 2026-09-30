package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/artifact"
	"stable/internal/core"
	"stable/internal/policy"
	"stable/internal/store"
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

type kindError string

func (e kindError) Error() string     { return "model request failed: " + string(e) }
func (e kindError) ErrorKind() string { return string(e) }

type failingAuditedDecider struct{ err error }

func (d failingAuditedDecider) Decide(context.Context, core.DecisionContext) (core.ProposedAction, error) {
	return core.ProposedAction{}, d.err
}
func (d failingAuditedDecider) Descriptor() core.ModelDescriptor {
	return core.ModelDescriptor{Provider: "openai-compatible", Model: "mock", Host: "127.0.0.1:9"}
}
func (d failingAuditedDecider) DecideModel(context.Context, core.DecisionContext) (core.ModelDecisionOutput, error) {
	return core.ModelDecisionOutput{}, d.err
}

func TestEvaluateRecordsModelFailureWithoutAction(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	a.Decider = failingAuditedDecider{err: kindError("rate_limited")}
	executed := false
	a.Executor = executorFunc(func(context.Context, string) (core.ActionRecord, error) {
		executed = true
		return core.ActionRecord{}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "timer-0")
	if err != nil || done || executed {
		t.Fatalf("result done=%v executed=%v err=%v", done, executed, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status != core.GoalNeedsHuman || !strings.Contains(snap.Goal.Reason, "rate_limited") {
		t.Fatalf("goal %+v", snap.Goal)
	}
	if len(snap.Actions) != 0 || len(snap.Decisions) != 0 {
		t.Fatalf("actions %+v decisions %+v", snap.Actions, snap.Decisions)
	}
	if len(snap.ModelCalls) != 1 {
		t.Fatalf("model calls %+v", snap.ModelCalls)
	}
	call := snap.ModelCalls[0]
	if call.Status != "failed" || call.ErrorKind != "rate_limited" || call.Provider != "openai-compatible" || call.Model != "mock" || call.FinishedAt == nil {
		t.Fatalf("model call %+v", call)
	}
}

func TestEvaluateInjectsUndeliveredConversation(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	stale, err := s.InsertMessage(ctx, core.SessionMessage{ID: "msg-old", GoalID: "g", Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: "earlier"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkMessagesDelivered(ctx, []string{stale.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-new", GoalID: "g", Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: "focus on J1 first"}); err != nil {
		t.Fatal(err)
	}
	var seen []core.SessionMessage
	a.Decider = deciderFunc(func(_ context.Context, d core.DecisionContext) (core.ProposedAction, error) {
		seen = d.Conversation
		return core.ProposedAction{Kind: "wait", Reason: "steered"}, nil
	})
	if _, err = a.EvaluateGoal(ctx, "g", "timer-0"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].ID != "msg-new" {
		t.Fatalf("conversation: %+v", seen)
	}
	seen = nil
	if _, err = a.EvaluateGoal(ctx, "g", "timer-1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("messages consumed twice: %+v", seen)
	}
}

func TestAskHumanRecordsQuestionAndReplyClearsIt(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	a.Decider = deciderFunc(func(_ context.Context, _ core.DecisionContext) (core.ProposedAction, error) {
		return core.ProposedAction{Kind: "ask_human", Reason: "which net should carry the sensor signal?"}, nil
	})
	if _, err := a.EvaluateGoal(ctx, "g", "timer-0"); err != nil {
		t.Fatal(err)
	}
	q, found, err := s.UnansweredQuestion(ctx, "g")
	if err != nil || !found || q.Role != core.MessageRoleAgent || q.Kind != core.MessageKindQuestion {
		t.Fatalf("question: %+v found=%v err=%v", q, found, err)
	}
	if _, err = s.InsertMessage(ctx, core.SessionMessage{ID: "msg-reply", GoalID: "g", Role: core.MessageRoleUser, Kind: core.MessageKindReply, Text: "J1.2"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = s.UnansweredQuestion(ctx, "g"); found {
		t.Fatal("question still open after reply")
	}
}

func TestVerifyRequiresAllCriteria(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	art, err := artifact.New(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	present := false
	a.Kicad = callerFunc(func(_ context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
		current, _ := art.Digest(ctx, path)
		result := core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID, ActualArtifactID: current}
		switch r.Kind {
		case "inspect_design":
			result.Status = "observed"
			result.Postcondition = json.RawMessage(`{"sensor.supported":true,"sensor.connection_present":` + boolJSON(present) + `}`)
		case "kicad.run_erc":
			var payload map[string]string
			_ = json.Unmarshal(r.Payload, &payload)
			_ = os.WriteFile(payload["report_path"], []byte(`{}`), 0644)
			result.Status = "pass"
			result.EvidencePaths = []string{payload["report_path"]}
		default:
			return result, errors.New("unknown capability")
		}
		return result, nil
	})
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	goal := snap.Goal
	goal.Criteria = []core.Criterion{
		{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	}
	if _, err = s.UpdateGoalCriteria(ctx, "g", goal.Criteria); err != nil {
		t.Fatal(err)
	}
	// ERC passes but the connection is still missing: not verified.
	if _, err = a.VerifyGoal(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	state, _ := s.GetGoalSnapshot(ctx, "g")
	if state.Goal.Status == core.GoalVerified {
		t.Fatal("verified despite unmet connection criterion")
	}
	// Connection restored: now verification completes.
	present = true
	if _, err = a.VerifyGoal(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	state, _ = s.GetGoalSnapshot(ctx, "g")
	if state.Goal.Status != core.GoalVerified {
		t.Fatalf("status %q reason %q", state.Goal.Status, state.Goal.Reason)
	}
	kinds := map[string]string{}
	for _, e := range state.Evidence {
		if e.Result == "pass" {
			kinds[e.Kind] = e.CriterionID
		}
	}
	if kinds["sensor.connection_present"] != "conn" || kinds["kicad.erc"] != "erc" {
		t.Fatalf("evidence kinds: %+v", kinds)
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
