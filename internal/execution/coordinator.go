package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/store"
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
	Permissions           PermissionGate
}

type PermissionGate interface {
	Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error)
}

type CandidateActionStore interface {
	SaveCandidate(context.Context, store.CandidateRecord) error
	GetCandidate(context.Context, string) (store.CandidateRecord, error)
	TransitionCandidate(context.Context, string, string, string, string) error
}

var ErrStaleDecision = errors.New("decision criteria revision is unknown or does not match the goal's current criteria revision")

// decisionStale reports whether the decision predates the goal's current
// criteria revision: a nil (legacy) or mismatched revision can never prove the
// proposal applies to the current criteria.
func decisionStale(goal core.Goal, decision core.Decision) bool {
	return decision.CriteriaRevision == nil || *decision.CriteriaRevision != goal.CriteriaRevision
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
		// Guard before reservation: a stale or legacy decision must not
		// prepare a new action (the store re-checks this in the reservation
		// transaction), and nothing is written to the artifact.
		if decisionStale(snap.Goal, decision) {
			return empty, fmt.Errorf("decision %s: %w", decisionID, ErrStaleDecision)
		}
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
	if action.Status == "applied" || action.Status == "verified" || action.Status == "blocked" || action.Status == "candidate_ready" || action.Status == "awaiting_accept" || action.Status == "awaiting_permission" {
		return action, nil
	}
	if action.Status != "prepared" && action.Status != "outcome_unknown" {
		return action, fmt.Errorf("invalid action state %q", action.Status)
	}
	return c.reconcileThenExecute(ctx, snap.Goal, decision, proposal, action, bridge)
}

func (c *Coordinator) reconcileThenExecute(ctx context.Context, g core.Goal, d core.Decision, p core.ProposedAction, a core.ActionRecord, bridge Caller) (core.ActionRecord, error) {
	// Guard before execution: the criteria may have changed after the action
	// was prepared; a stale decision's action is blocked without touching the
	// artifact (completed actions stay as history for the new round to
	// re-evaluate).
	fresh, err := c.Store.GetGoalSnapshot(ctx, g.ID)
	if err != nil {
		return a, err
	}
	if decisionStale(fresh.Goal, d) {
		reason := "decision criteria revision is stale or unknown; action blocked without modifying the artifact"
		if err = c.Store.SetActionResult(ctx, a.ID, "blocked", a.ResultArtifactID, reason); err != nil {
			return a, err
		}
		a.Status = "blocked"
		a.Reason = reason
		return a, nil
	}
	repo, ok := c.Store.(CandidateActionStore)
	if !ok {
		return a, errors.New("candidate storage is not configured; legacy action refused")
	}
	candidateID := "candidate-" + a.ID
	stored, loadErr := repo.GetCandidate(ctx, candidateID)
	hasCandidate := loadErr == nil
	if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
		return a, loadErr
	}
	if hasCandidate && (stored.Candidate.Status == "ready" || stored.Candidate.Status == "reviewed" || stored.Candidate.Status == "accepted") {
		a.Status = "candidate_ready"
		a.ResultArtifactID = stored.Candidate.CandidateDigest
		return a, nil
	}
	if !hasCandidate {
		if err = validateProjectTarget(g.AllowedRoot, p.Target); err != nil {
			return a, err
		}
		parent := filepath.Join(filepath.Dir(g.AllowedRoot), ".stable-candidates")
		created, createErr := candidate.CreateCandidate(candidateID, g.AllowedRoot, parent)
		if createErr != nil {
			return a, createErr
		}
		stored = store.CandidateRecord{Candidate: created, ActionID: a.ID, GoalID: g.ID}
		if err = repo.SaveCandidate(ctx, stored); err != nil {
			return a, err
		}
		hasCandidate = true
	}
	formalPayload, err := bridgePayload(p.Target, g.AllowedRoot, g.AllowedRoot, stored.Candidate.CandidateRoot, filepath.Join(filepath.Dir(g.AllowedRoot), ".stable-runs", candidateID))
	if err != nil {
		return a, err
	}
	observeReq := core.CapabilityRequest{ProtocolVersion: 1, OperationID: a.ID + "-inspect", Kind: "inspect_design", GoalID: g.ID, Payload: formalPayload}
	observed, err := bridge.Call(ctx, observeReq)
	if err != nil {
		_ = c.Store.SetActionResult(ctx, a.ID, "outcome_unknown", "", err.Error())
		a.Status = "outcome_unknown"
		a.Reason = err.Error()
		return a, err
	}
	current, err := c.Artifacts.Digest(ctx, p.Target)
	if err != nil {
		return a, err
	}
	if current != a.ExpectedArtifactID {
		reason := "formal project changed before candidate execution; candidate cannot be accepted against a stale baseline"
		if stored.Candidate.Status == "prepared" {
			_ = repo.TransitionCandidate(ctx, candidateID, "prepared", "blocked", "")
		}
		_ = c.Store.SetActionResult(ctx, a.ID, "blocked", current, reason)
		a.Status = "blocked"
		a.ResultArtifactID = current
		a.Reason = reason
		return a, nil
	}
	if observed.Status == "observed" && matches(a.DesiredPostcondition, observed.Postcondition) {
		if stored.Candidate.Status == "prepared" {
			_ = repo.TransitionCandidate(ctx, candidateID, "prepared", "blocked", "")
		}
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
	if stored.Candidate.Status == "running" {
		candidateTarget, relErr := candidateTargetPath(g.AllowedRoot, stored.Candidate.CandidateRoot, p.Target)
		if relErr != nil {
			return a, relErr
		}
		payload, err := bridgePayload(candidateTarget, stored.Candidate.CandidateRoot, g.AllowedRoot, stored.Candidate.CandidateRoot, filepath.Join(filepath.Dir(g.AllowedRoot), ".stable-runs", candidateID))
		if err != nil {
			return a, err
		}
		check, inspectErr := bridge.Call(ctx, core.CapabilityRequest{ProtocolVersion: 1, OperationID: a.ID + "-reconcile", Kind: "inspect_design", GoalID: g.ID, Payload: payload})
		if inspectErr != nil {
			a.Status = "outcome_unknown"
			a.Reason = inspectErr.Error()
			return a, inspectErr
		}
		if check.Status == "observed" && matches(a.DesiredPostcondition, check.Postcondition) {
			return c.markCandidateReady(ctx, repo, stored, a)
		}
		reason := "candidate effect is uncertain; automatic replay refused"
		_ = repo.TransitionCandidate(ctx, candidateID, "running", "blocked", "")
		_ = c.Store.SetActionResult(ctx, a.ID, "blocked", stored.Candidate.CandidateDigest, reason)
		a.Status = "blocked"
		a.Reason = reason
		return a, nil
	}
	if c.Permissions == nil {
		return a, errors.New("permission gate is unavailable; legacy bridge action refused")
	}
	candidateTarget, err := candidateTargetPath(g.AllowedRoot, stored.Candidate.CandidateRoot, p.Target)
	if err != nil {
		return a, err
	}
	sessionID := g.SourceSessionID
	mode := permission.ModeDefault
	if sessionID == "" {
		// Legacy agentctl goals predate conversation ownership. Keep their
		// historical non-interactive execution while still applying the same
		// candidate-root hard boundary through the permission gate.
		sessionID = "legacy-" + g.ID
		mode = permission.ModeAcceptEdits
	}
	authority := permission.Authority{RunID: a.ID, SessionID: sessionID, GoalID: g.ID, WorkItemID: a.ID, AllowedRoot: g.AllowedRoot, FormalRoot: g.AllowedRoot, CandidateRoot: stored.Candidate.CandidateRoot, Mode: mode, Capabilities: append([]string(nil), g.AllowedCapabilities...)}
	permissionOperation := permission.Operation{ID: a.ID + "-legacy-write", Kind: permission.OpLegacy, Name: p.Capability, Target: candidateTarget, Parameters: append(json.RawMessage(nil), p.Parameters...)}
	permissionDecision, err := c.Permissions.Authorize(ctx, authority, permissionOperation)
	if err != nil {
		return a, fmt.Errorf("authorize legacy action: %w", err)
	}
	if permissionDecision.Kind == permission.DecisionAsk {
		reason := fmt.Sprintf("awaiting user approval %s", permissionDecision.ApprovalID)
		if err = c.Store.SetActionResult(ctx, a.ID, "awaiting_permission", "", reason); err != nil {
			return a, err
		}
		a.Status, a.Reason = "awaiting_permission", reason
		return a, nil
	}
	if permissionDecision.Kind != permission.DecisionAllow {
		_ = repo.TransitionCandidate(ctx, candidateID, stored.Candidate.Status, "blocked", "")
		_ = c.Store.SetActionResult(ctx, a.ID, "blocked", "", permissionDecision.Reason)
		a.Status, a.Reason = "blocked", permissionDecision.Reason
		return a, nil
	}
	if stored.Candidate.Status != "prepared" {
		return a, fmt.Errorf("candidate %s cannot run from state %q", candidateID, stored.Candidate.Status)
	}
	if err = repo.TransitionCandidate(ctx, candidateID, "prepared", "running", ""); err != nil {
		return a, err
	}
	stored.Candidate.Status = "running"
	candidateTarget, err = candidateTargetPath(g.AllowedRoot, stored.Candidate.CandidateRoot, p.Target)
	if err != nil {
		return a, err
	}
	payload, err := bridgePayload(candidateTarget, stored.Candidate.CandidateRoot, g.AllowedRoot, stored.Candidate.CandidateRoot, filepath.Join(filepath.Dir(g.AllowedRoot), ".stable-runs", candidateID))
	if err != nil {
		return a, err
	}
	request := core.CapabilityRequest{ProtocolVersion: 1, OperationID: a.ID, Kind: p.Capability, GoalID: g.ID, ExpectedArtifactID: a.ExpectedArtifactID, Payload: payload}
	result, err := bridge.Call(ctx, request)
	if err != nil {
		_ = c.Store.SetActionResult(ctx, a.ID, "outcome_unknown", "", err.Error())
		a.Status = "outcome_unknown"
		a.Reason = err.Error()
		return a, err
	}
	actual, err := c.Artifacts.Digest(ctx, candidateTarget)
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
		return c.markCandidateReady(ctx, repo, stored, a)
	}
	reason := fmt.Sprintf("capability status %s; desired postcondition absent", result.Status)
	_ = repo.TransitionCandidate(ctx, candidateID, "running", "blocked", "")
	if err = c.Store.SetActionResult(ctx, a.ID, "blocked", actual, reason); err != nil {
		return a, err
	}
	a.Status = "blocked"
	a.ResultArtifactID = actual
	a.Reason = reason
	return a, nil
}

func (c *Coordinator) markCandidateReady(ctx context.Context, repo CandidateActionStore, record store.CandidateRecord, a core.ActionRecord) (core.ActionRecord, error) {
	frozen, err := candidate.FreezeCandidate(record.Candidate, nil, ctx)
	if err != nil {
		return a, err
	}
	if err = repo.TransitionCandidate(ctx, record.Candidate.ID, record.Candidate.Status, "ready", frozen.CandidateDigest); err != nil {
		return a, err
	}
	if err = c.Store.SetActionResult(ctx, a.ID, "candidate_ready", frozen.CandidateDigest, "candidate is ready for user review"); err != nil {
		return a, err
	}
	a.Status = "candidate_ready"
	a.ResultArtifactID = frozen.CandidateDigest
	a.Reason = "candidate is ready for user review"
	return a, nil
}

func validateProjectTarget(root, target string) error {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	p, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("legacy target is outside authorized project")
	}
	return nil
}

func candidateTargetPath(formalRoot, candidateRoot, target string) (string, error) {
	if err := validateProjectTarget(formalRoot, target); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(formalRoot, target)
	if err != nil {
		return "", err
	}
	clean, err := candidate.CleanRelative(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(candidateRoot, clean), nil
}

func bridgePayload(path, allowedRoot, projectRoot, candidateRoot, runRoot string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"path": path, "allowed_root": allowedRoot, "project_root": projectRoot, "candidate_root": candidateRoot, "run_root": runRoot})
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
