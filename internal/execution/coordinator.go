package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"stable/internal/core"
)

type Caller interface {
	Call(context.Context, core.CapabilityRequest) (core.CapabilityResult, error)
}

type ActionStore interface {
	GoalIDForDecision(context.Context, string) (string, error)
	GetGoalSnapshot(context.Context, string) (core.GoalSnapshot, error)
	ReserveAction(context.Context, core.ActionRecord) (core.ActionRecord, error)
	SetActionResult(context.Context, string, string, string, string) error
}

type Coordinator struct {
	Store                 ActionStore
	Artifacts             core.ArtifactStore
	Policy                core.ActionPolicy
	Capabilities          map[string]Caller
	Postconditions        map[string]json.RawMessage
	AfterCapabilityEffect func()
}

func (c *Coordinator) ExecuteOrReconcile(ctx context.Context, decisionID string) (core.ActionRecord, error) {
	var empty core.ActionRecord
	goalID, err := c.Store.GoalIDForDecision(ctx, decisionID)
	if err != nil {
		return empty, err
	}
	snap, err := c.Store.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return empty, err
	}
	var decision core.Decision
	found := false
	for _, d := range snap.Decisions {
		if d.ID == decisionID {
			decision = d
			found = true
			break
		}
	}
	if !found {
		return empty, errors.New("decision not found")
	}
	var observed core.Observation
	found = false
	for _, o := range snap.Observations {
		if o.ID == decision.ObservationID {
			observed = o
			found = true
			break
		}
	}
	if !found {
		return empty, errors.New("decision observation not found")
	}
	proposal := decision.Proposal
	if proposal.Kind != "execute_capability" {
		return empty, errors.New("decision is not an executable capability")
	}
	bridge := c.Capabilities[proposal.Capability]
	if bridge == nil {
		return empty, fmt.Errorf("capability %q unavailable", proposal.Capability)
	}
	actionID := "action-" + decisionID
	var action core.ActionRecord
	found = false
	for _, a := range snap.Actions {
		if a.ID == actionID {
			action = a
			found = true
			break
		}
	}
	if !found {
		if err = c.Policy.Check(snap.Goal, observed, proposal); err != nil {
			return empty, err
		}
		current, err := c.Artifacts.Digest(ctx, proposal.Target)
		if err != nil {
			return empty, err
		}
		if current != proposal.ExpectedArtifactID {
			return empty, errors.New("design changed since proposal")
		}
		if _, err = c.Artifacts.Snapshot(ctx, goalID, proposal.Target); err != nil {
			return empty, err
		}
		post := c.Postconditions[proposal.Capability]
		if len(post) == 0 {
			return empty, errors.New("capability postcondition missing")
		}
		action, err = c.Store.ReserveAction(ctx, core.ActionRecord{ID: actionID, DecisionID: decisionID, ExpectedArtifactID: current, DesiredPostcondition: post})
		if err != nil {
			return empty, err
		}
	}
	if action.Status == "applied" || action.Status == "verified" || action.Status == "blocked" {
		return action, nil
	}
	if action.Status != "prepared" && action.Status != "outcome_unknown" {
		return action, fmt.Errorf("invalid action state %q", action.Status)
	}
	return c.reconcileThenExecute(ctx, snap.Goal, proposal, action, bridge)
}

func (c *Coordinator) reconcileThenExecute(ctx context.Context, g core.Goal, p core.ProposedAction, a core.ActionRecord, bridge Caller) (core.ActionRecord, error) {
	payload := json.RawMessage(fmt.Sprintf(`{"path":%q,"allowed_root":%q}`, p.Target, g.AllowedRoot))
	observeReq := core.CapabilityRequest{ProtocolVersion: 1, OperationID: a.ID + "-inspect", Kind: "inspect_design", GoalID: g.ID, Payload: payload}
	observed, err := bridge.Call(ctx, observeReq)
	if err != nil {
		_ = c.Store.SetActionResult(ctx, a.ID, "outcome_unknown", "", err.Error())
		a.Status = "outcome_unknown"
		a.Reason = err.Error()
		return a, err
	}
	if observed.Status == "observed" && matches(a.DesiredPostcondition, observed.Postcondition) {
		actual, err := c.Artifacts.Digest(ctx, p.Target)
		if err != nil {
			return a, err
		}
		if err = c.Store.SetActionResult(ctx, a.ID, "applied", actual, "postcondition already satisfied"); err != nil {
			return a, err
		}
		a.Status = "applied"
		a.ResultArtifactID = actual
		a.Reason = "postcondition already satisfied"
		return a, nil
	}
	current, err := c.Artifacts.Digest(ctx, p.Target)
	if err != nil {
		return a, err
	}
	if current != a.ExpectedArtifactID {
		reason := "design changed and postcondition is not satisfied"
		_ = c.Store.SetActionResult(ctx, a.ID, "blocked", current, reason)
		a.Status = "blocked"
		a.ResultArtifactID = current
		a.Reason = reason
		return a, nil
	}
	request := core.CapabilityRequest{ProtocolVersion: 1, OperationID: a.ID, Kind: p.Capability, GoalID: g.ID, ExpectedArtifactID: a.ExpectedArtifactID, Payload: payload}
	result, err := bridge.Call(ctx, request)
	if err != nil {
		_ = c.Store.SetActionResult(ctx, a.ID, "outcome_unknown", "", err.Error())
		a.Status = "outcome_unknown"
		a.Reason = err.Error()
		return a, err
	}
	actual, err := c.Artifacts.Digest(ctx, p.Target)
	if err != nil {
		return a, err
	}
	if result.ActualArtifactID != actual {
		err = errors.New("capability result digest does not match design")
		_ = c.Store.SetActionResult(ctx, a.ID, "outcome_unknown", actual, err.Error())
		a.Status = "outcome_unknown"
		a.ResultArtifactID = actual
		a.Reason = err.Error()
		return a, err
	}
	if c.AfterCapabilityEffect != nil && result.Status == "applied" {
		c.AfterCapabilityEffect()
	}
	if (result.Status == "applied" || result.Status == "already_satisfied") && matches(a.DesiredPostcondition, result.Postcondition) {
		if err = c.Store.SetActionResult(ctx, a.ID, "applied", actual, ""); err != nil {
			return a, err
		}
		a.Status = "applied"
		a.ResultArtifactID = actual
		return a, nil
	}
	reason := fmt.Sprintf("capability status %s; desired postcondition absent", result.Status)
	if err = c.Store.SetActionResult(ctx, a.ID, "blocked", actual, reason); err != nil {
		return a, err
	}
	a.Status = "blocked"
	a.ResultArtifactID = actual
	a.Reason = reason
	return a, nil
}

func matches(desired, actual json.RawMessage) bool {
	var want, got map[string]any
	if json.Unmarshal(desired, &want) != nil || json.Unmarshal(actual, &got) != nil {
		return false
	}
	if len(want) == 0 {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
