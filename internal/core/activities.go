package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CapabilityCaller interface {
	Call(context.Context, CapabilityRequest) (CapabilityResult, error)
}

type ActionExecutor interface {
	ExecuteOrReconcile(context.Context, string) (ActionRecord, error)
}

type Activities struct {
	State     StateStore
	Artifacts ArtifactStore
	Kicad     CapabilityCaller
	Computer  CapabilityCaller
	Decider   DecisionMaker
	Policy    ActionPolicy
	Executor  ActionExecutor
}

func (a *Activities) CheckInterval(ctx context.Context, goalID string) (int, error) {
	s, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return 0, err
	}
	if s.Goal.CheckIntervalSeconds <= 0 {
		return 30, nil
	}
	return s.Goal.CheckIntervalSeconds, nil
}

func (a *Activities) ObserveGoal(ctx context.Context, goalID, eventID string) (Observation, error) {
	var out Observation
	snap, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return out, err
	}
	g := snap.Goal
	if g.ArtifactPath == "" {
		return out, errors.New("goal artifact path missing")
	}
	current, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return out, err
	}
	if current != g.CurrentArtifactID {
		if err = a.State.SetCurrentArtifact(ctx, goalID, current); err != nil {
			return out, err
		}
		if err = a.State.InvalidateEvidence(ctx, goalID, current); err != nil {
			return out, err
		}
		g.CurrentArtifactID = current
	}
	payload := requestPayload(g, nil)
	design, err := a.Kicad.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: uniqueID("inspect"), Kind: "inspect_design", GoalID: goalID, ExpectedArtifactID: current, Payload: payload})
	if err != nil {
		return out, err
	}
	if design.Status != "observed" || design.ActualArtifactID != current {
		return out, errors.New("design observation does not match current digest")
	}
	computerFacts := map[string]any{"status": "absent", "session_id": snap.Session.ID, "generation": snap.Session.Generation}
	if snap.Session.Generation > 0 {
		computer, err := a.Computer.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: uniqueID("computer-observe"), Kind: "computer.observe", GoalID: goalID, ExpectedArtifactID: current, Payload: requestPayload(g, &snap.Session)})
		if err == nil {
			var observed ComputerObservation
			if json.Unmarshal(computer.Postcondition, &observed) == nil {
				computerFacts = map[string]any{"status": observed.Status, "session_id": observed.SessionID, "generation": observed.Generation, "window_identity": observed.WindowIdentity, "screenshot_path": observed.ScreenshotPath}
				if observed.Status == "stale" {
					snap.Session.Status = "stale"
					_ = a.State.UpsertSession(ctx, snap.Session)
				}
			}
		}
	}
	var designFacts any
	if err = json.Unmarshal(design.Postcondition, &designFacts); err != nil {
		return out, err
	}
	facts, _ := json.Marshal(map[string]any{"design": designFacts, "computer": computerFacts, "artifact_path": g.ArtifactPath})
	out = Observation{ID: uniqueID("observation"), GoalID: goalID, EventID: eventID, ArtifactID: current, ComputerSessionID: snap.Session.ID, Facts: facts, ObservedAt: time.Now().UTC()}
	if err = a.State.RecordObservation(ctx, out); err != nil {
		return Observation{}, err
	}
	return out, nil
}

func (a *Activities) VerifyGoal(ctx context.Context, goalID string) (Evidence, error) {
	var empty Evidence
	snap, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return empty, err
	}
	g := snap.Goal
	before, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return empty, err
	}
	if before != g.CurrentArtifactID {
		return empty, errors.New("design changed before ERC")
	}
	report := filepath.Join(g.AllowedRoot, "reports", "erc-"+before+".json")
	if err = os.MkdirAll(filepath.Dir(report), 0755); err != nil {
		return empty, err
	}
	payload := requestPayload(g, nil)
	var props map[string]any
	if err = json.Unmarshal(payload, &props); err != nil {
		return empty, err
	}
	props["report_path"] = report
	payload, _ = json.Marshal(props)
	result, err := a.Kicad.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: uniqueID("erc"), Kind: "kicad.run_erc", GoalID: goalID, ExpectedArtifactID: before, Payload: payload})
	if err != nil {
		return empty, err
	}
	after, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return empty, err
	}
	if before != after || result.ActualArtifactID != after {
		return empty, errors.New("design changed during ERC")
	}
	if result.Status != "pass" && result.Status != "fail" {
		return empty, fmt.Errorf("ERC unavailable: %s", result.Status)
	}
	criterion := "erc"
	for _, item := range g.Criteria {
		if item.Kind == "kicad.erc_clean" {
			criterion = item.ID
			break
		}
	}
	if len(result.EvidencePaths) != 1 {
		return empty, errors.New("ERC report path missing")
	}
	e := Evidence{ID: uniqueID("evidence"), GoalID: goalID, CriterionID: criterion, ArtifactID: after, Kind: "kicad.erc", Result: result.Status, ReportPath: result.EvidencePaths[0], CreatedAt: time.Now().UTC()}
	if err = a.State.RecordEvidence(ctx, e); err != nil {
		return empty, err
	}
	if e.Result != "pass" {
		return e, nil
	}
	// Remaining criteria must pass too before the goal can be verified.
	for _, item := range g.Criteria {
		switch item.Kind {
		case "kicad.erc_clean":
		case "sensor.connection_present":
			check, err := a.verifyConnection(ctx, goalID, g, item, after)
			if err != nil {
				return e, err
			}
			if check.Result != "pass" {
				return e, nil
			}
		default:
			return e, fmt.Errorf("criterion %s: unsupported kind %q", item.ID, item.Kind)
		}
	}
	latest, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return e, err
	}
	actual, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return e, err
	}
	if latest.Goal.CurrentArtifactID != after || actual != after {
		return e, errors.New("design changed after ERC")
	}
	_, err = a.State.UpdateStatus(ctx, goalID, latest.Goal.Revision, GoalVerified, "all acceptance criteria passed against the current design")
	return e, err
}

// verifyConnection checks the requested sensor connection against fresh
// design facts and records objective evidence bound to the artifact digest.
func (a *Activities) verifyConnection(ctx context.Context, goalID string, g Goal, item Criterion, digest string) (Evidence, error) {
	empty := Evidence{}
	design, err := a.Kicad.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: uniqueID("inspect"), Kind: "inspect_design", GoalID: goalID, ExpectedArtifactID: digest, Payload: requestPayload(g, nil)})
	if err != nil {
		return empty, err
	}
	if design.ActualArtifactID != digest {
		return empty, errors.New("design changed during criteria verification")
	}
	var facts struct {
		ConnectionPresent bool `json:"sensor.connection_present"`
	}
	if err = json.Unmarshal(design.Postcondition, &facts); err != nil {
		return empty, err
	}
	result := "fail"
	if design.Status == "observed" && facts.ConnectionPresent {
		result = "pass"
	}
	e := Evidence{ID: uniqueID("evidence"), GoalID: goalID, CriterionID: item.ID, ArtifactID: digest, Kind: "sensor.connection_present", Result: result, CreatedAt: time.Now().UTC()}
	if design.Status == "unsupported" {
		e.Result = "fail"
		e.ReportPath = ""
	}
	return e, a.State.RecordEvidence(ctx, e)
}

func (a *Activities) EvaluateGoal(ctx context.Context, goalID, eventID string) (bool, error) {
	snap, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return false, err
	}
	kind := "timer"
	if !strings.HasPrefix(eventID, "timer-") {
		kind = "external"
	}
	event, inserted, err := a.State.InsertEventIfAbsent(ctx, Event{ID: eventID, GoalID: goalID, Kind: kind, Payload: json.RawMessage(`{}`)})
	if err != nil {
		return false, err
	}
	if !inserted && event.Status == "processed" {
		return snap.Goal.Status == GoalVerified, nil
	}
	if event.Status == "pending" {
		_ = a.State.SetEventStatus(ctx, eventID, "signaled")
	}
	finish := func(done bool, err error) (bool, error) {
		if err == nil {
			_ = a.State.SetEventStatus(ctx, eventID, "processed")
		}
		return done, err
	}
	for _, action := range snap.Actions {
		if action.Status == "prepared" || action.Status == "outcome_unknown" {
			_, _ = a.Executor.ExecuteOrReconcile(ctx, action.DecisionID)
		}
	}
	observed, err := a.ObserveGoal(ctx, goalID, eventID)
	if err != nil {
		return finish(false, err)
	}
	snap, err = a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return finish(false, err)
	}
	valid := make([]Evidence, 0)
	for _, e := range snap.Evidence {
		if e.Result == "pass" && e.ArtifactID == observed.ArtifactID {
			valid = append(valid, e)
		}
	}
	conversation, err := a.State.UndeliveredMessages(ctx, goalID)
	if err != nil {
		return finish(false, err)
	}
	ctxForModel := DecisionContext{Goal: snap.Goal, Agent: snap.Agent, Observation: observed, ValidEvidence: valid, Capabilities: []CapabilityDescriptor{
		{Name: "kicad.repair_connection", PostconditionKind: "sensor.connection_present"},
		{Name: "computer.ensure_open", PostconditionKind: "computer.open"},
	}, Conversation: conversation}
	var proposal ProposedAction
	var modelRunID string
	var modelCallID string
	if modeled, ok := a.Decider.(AuditedDecisionMaker); ok {
		audit, ok := a.State.(ModelCallStore)
		if !ok {
			return finish(false, errors.New("model call audit store unavailable"))
		}
		modelCallID = uniqueID("model-call")
		d := modeled.Descriptor()
		if err = audit.StartModelCall(ctx, ModelCall{ID: modelCallID, GoalID: goalID, ObservationID: observed.ID, Provider: d.Provider, Model: d.Model, Host: d.Host}); err != nil {
			return finish(false, err)
		}
		var output ModelDecisionOutput
		output, err = modeled.DecideModel(ctx, ctxForModel)
		status, kind := "succeeded", ""
		if err != nil {
			status = "failed"
			kind = "model_error"
			if classified, ok := err.(interface{ ErrorKind() string }); ok {
				kind = classified.ErrorKind()
			}
		}
		if finishErr := audit.FinishModelCall(ctx, modelCallID, status, output.ProviderRequestID, kind); finishErr != nil {
			return finish(false, finishErr)
		}
		proposal = output.Proposal
	} else if detailed, ok := a.Decider.(interface {
		DecideDetailed(context.Context, DecisionContext) (ProposedAction, string, error)
	}); ok {
		proposal, modelRunID, err = detailed.DecideDetailed(ctx, ctxForModel)
	} else {
		proposal, err = a.Decider.Decide(ctx, ctxForModel)
	}
	if err != nil {
		_ = a.setStatus(ctx, goalID, GoalNeedsHuman, "model unavailable: "+err.Error())
		return finish(false, nil)
	}
	decision := Decision{ID: uniqueID("decision"), AgentID: snap.Agent.ID, ObservationID: observed.ID, Proposal: proposal, ModelRunID: modelRunID, ModelCallID: modelCallID, CreatedAt: time.Now().UTC()}
	if err = a.State.RecordDecision(ctx, decision); err != nil {
		return finish(false, err)
	}
	if len(conversation) > 0 {
		ids := make([]string, 0, len(conversation))
		for _, m := range conversation {
			ids = append(ids, m.ID)
		}
		if err = a.State.MarkMessagesDelivered(ctx, ids); err != nil {
			return finish(false, err)
		}
	}
	switch proposal.Kind {
	case "execute_capability":
		if err = a.Policy.Check(snap.Goal, observed, proposal); err != nil {
			_ = a.setStatus(ctx, goalID, GoalNeedsHuman, "policy rejected: "+err.Error())
			return finish(false, nil)
		}
		action, err := a.Executor.ExecuteOrReconcile(ctx, decision.ID)
		if err != nil {
			_ = a.setStatus(ctx, goalID, GoalWaiting, "action outcome unknown: "+err.Error())
			return finish(false, nil)
		}
		if action.Status != "applied" {
			_ = a.setStatus(ctx, goalID, GoalNeedsHuman, action.Reason)
			return finish(false, nil)
		}
		if _, err = a.ObserveGoal(ctx, goalID, eventID); err != nil {
			return finish(false, err)
		}
		e, err := a.VerifyGoal(ctx, goalID)
		if err != nil {
			_ = a.setStatus(ctx, goalID, GoalWaiting, "verification failed: "+err.Error())
			return finish(false, nil)
		}
		done, err := a.finishIfVerified(ctx, goalID, e)
		return finish(done, err)
	case "open_computer":
		if err = a.Policy.Check(snap.Goal, observed, proposal); err != nil {
			_ = a.setStatus(ctx, goalID, GoalNeedsHuman, "policy rejected: "+err.Error())
			return finish(false, nil)
		}
		action, err := a.State.ReserveAction(ctx, ActionRecord{ID: "action-" + decision.ID, DecisionID: decision.ID, ExpectedArtifactID: observed.ArtifactID, DesiredPostcondition: json.RawMessage(`{"status":"open"}`)})
		if err != nil {
			return finish(false, err)
		}
		requestKind := "computer.ensure_open"
		if snap.Session.Generation > 0 {
			requestKind = "computer.recover"
		}
		result, err := a.Computer.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: action.ID, Kind: requestKind, GoalID: goalID, ExpectedArtifactID: observed.ArtifactID, Payload: requestPayload(snap.Goal, &snap.Session)})
		if err != nil {
			_ = a.State.SetActionResult(ctx, action.ID, "outcome_unknown", "", err.Error())
			return finish(false, nil)
		}
		var computer ComputerObservation
		if err = json.Unmarshal(result.Postcondition, &computer); err != nil {
			return finish(false, err)
		}
		if result.Status != "applied" || computer.Status != "open" {
			_ = a.State.SetActionResult(ctx, action.ID, "blocked", "", "computer did not open")
			return finish(false, nil)
		}
		session := ComputerSession{ID: snap.Session.ID, GoalID: goalID, Status: "open", Generation: computer.Generation, OpenedArtifactID: computer.OpenedArtifactID, LastObservationID: observed.ID, RuntimeHandle: computer.RuntimeHandle}
		if err = a.State.UpsertSession(ctx, session); err != nil {
			return finish(false, err)
		}
		if computer.ScreenshotPath != "" {
			_ = a.State.RecordEvidence(ctx, Evidence{ID: uniqueID("screenshot"), GoalID: goalID, CriterionID: "computer", ArtifactID: observed.ArtifactID, Kind: "computer.screenshot", Result: "pass", ReportPath: computer.ScreenshotPath})
		}
		_ = a.State.SetActionResult(ctx, action.ID, "applied", observed.ArtifactID, "")
		_ = a.setStatus(ctx, goalID, GoalWaiting, "computer session opened")
	case "observe":
		e, err := a.VerifyGoal(ctx, goalID)
		if err != nil {
			_ = a.setStatus(ctx, goalID, GoalWaiting, "verification unavailable: "+err.Error())
			return finish(false, nil)
		}
		done, err := a.finishIfVerified(ctx, goalID, e)
		return finish(done, err)
	case "wait":
		_ = a.setStatus(ctx, goalID, GoalWaiting, proposal.Reason)
	case "ask_human":
		if _, err = a.State.InsertMessage(ctx, SessionMessage{ID: uniqueID("msg"), GoalID: goalID, Role: MessageRoleAgent, Kind: MessageKindQuestion, Text: proposal.Reason}); err != nil {
			return finish(false, err)
		}
		_ = a.setStatus(ctx, goalID, GoalNeedsHuman, proposal.Reason)
	default:
		_ = a.setStatus(ctx, goalID, GoalNeedsHuman, "unsupported model action: "+proposal.Kind)
	}
	return finish(false, nil)
}

// finishIfVerified reports workflow completion only when the goal status is
// verified; otherwise it names the criteria that are still unmet.
func (a *Activities) finishIfVerified(ctx context.Context, goalID string, e Evidence) (bool, error) {
	latest, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return false, err
	}
	if latest.Goal.Status == GoalVerified && e.Result == "pass" {
		return true, nil
	}
	_ = a.setStatus(ctx, goalID, GoalNeedsHuman, unmetCriteriaReason(latest.Evidence, e.ArtifactID))
	return false, nil
}

func unmetCriteriaReason(evidence []Evidence, artifactID string) string {
	latest := map[string]string{}
	for _, e := range evidence {
		if e.ArtifactID == artifactID {
			latest[e.CriterionID] = e.Result
		}
	}
	var unmet []string
	for id, res := range latest {
		if res != "pass" {
			unmet = append(unmet, id)
		}
	}
	if len(unmet) == 0 {
		return "acceptance criteria not yet satisfied"
	}
	sort.Strings(unmet)
	return "unmet criteria: " + strings.Join(unmet, ", ")
}

// WaitingForHuman reports whether an agent question is still unanswered; the
// workflow uses it to stop timer-driven re-evaluation while a human reply is
// pending.
func (a *Activities) WaitingForHuman(ctx context.Context, goalID string) (bool, error) {
	_, found, err := a.State.UnansweredQuestion(ctx, goalID)
	return found, err
}

func (a *Activities) setStatus(ctx context.Context, id string, status GoalStatus, reason string) error {
	s, err := a.State.GetGoalSnapshot(ctx, id)
	if err != nil {
		return err
	}
	_, err = a.State.UpdateStatus(ctx, id, s.Goal.Revision, status, reason)
	return err
}

func requestPayload(g Goal, session *ComputerSession) json.RawMessage {
	data := map[string]any{"path": g.ArtifactPath, "allowed_root": g.AllowedRoot}
	if session != nil {
		data["session"] = session
	}
	b, _ := json.Marshal(data)
	return b
}

func uniqueID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano()) }
