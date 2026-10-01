package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// evidenceInvalidationRule is recorded in every new evidence row's provenance.
const evidenceInvalidationRule = "标准变更或产物摘要变化时失效"

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

// VerifyGoal runs one full reverification round against a single token (the
// goal's criteria revision and current artifact at read time): every current
// ERC and connection criterion gets its own provenance-backed evidence from
// one shared ERC report, and the round is committed once via
// CommitVerification. It never sets verified mid-check; current reports
// whether the token still matched at commit time.
func (a *Activities) VerifyGoal(ctx context.Context, goalID string) (VerificationResult, bool, error) {
	var out VerificationResult
	snap, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return out, false, err
	}
	g := snap.Goal
	before, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return out, false, err
	}
	if before != g.CurrentArtifactID {
		return out, false, errors.New("design changed before checks")
	}
	token := VerificationToken{GoalID: goalID, CriteriaRevision: g.CriteriaRevision, ArtifactID: before}
	revision := g.CriteriaRevision

	var ercCriteria, connectionCriteria []Criterion
	for _, item := range g.Criteria {
		switch item.Kind {
		case CriterionKindERCClean:
			ercCriteria = append(ercCriteria, item)
		case CriterionKindConnectionPresent:
			connectionCriteria = append(connectionCriteria, item)
		default:
			return out, false, fmt.Errorf("criterion %s: unsupported kind %q", item.ID, item.Kind)
		}
	}

	if len(ercCriteria) > 0 {
		// One ERC run serves every ERC criterion; the request threshold is the
		// strictest, and each criterion is judged against its own threshold
		// from the report's violation count.
		threshold := ercThreshold(ercCriteria[0])
		for _, item := range ercCriteria[1:] {
			if t := ercThreshold(item); t < threshold {
				threshold = t
			}
		}
		checkID := uniqueID("erc")
		report := filepath.Join(g.AllowedRoot, "reports", fmt.Sprintf("erc-v%d-%s-%s.json", revision, checkID, before))
		if err = os.MkdirAll(filepath.Dir(report), 0755); err != nil {
			return out, false, err
		}
		var props map[string]any
		if err = json.Unmarshal(requestPayload(g, nil), &props); err != nil {
			return out, false, err
		}
		props["report_path"] = report
		props["max_violations"] = threshold
		payload, _ := json.Marshal(props)
		result, err := a.Kicad.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: checkID, Kind: "kicad.run_erc", GoalID: goalID, ExpectedArtifactID: before, Payload: payload})
		if err != nil {
			return out, false, err
		}
		after, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
		if err != nil {
			return out, false, err
		}
		if before != after || result.ActualArtifactID != after {
			return out, false, errors.New("design changed during ERC")
		}
		if result.Status != "pass" && result.Status != "fail" {
			return out, false, fmt.Errorf("ERC unavailable: %s", result.Status)
		}
		if len(result.EvidencePaths) != 1 {
			return out, false, errors.New("ERC report path missing")
		}
		var facts struct {
			ViolationCount *int   `json:"violation_count"`
			CheckerID      string `json:"checker_id"`
			CheckerVersion string `json:"checker_version"`
		}
		if err = json.Unmarshal(result.Postcondition, &facts); err != nil {
			return out, false, err
		}
		if facts.ViolationCount == nil || facts.CheckerID == "" || facts.CheckerVersion == "" {
			return out, false, errors.New("ERC result lacks checker identity or violation count")
		}
		for _, item := range ercCriteria {
			itemThreshold := ercThreshold(item)
			res := "fail"
			if *facts.ViolationCount <= itemThreshold {
				res = "pass"
			}
			out.Evidence = append(out.Evidence, Evidence{
				ID: uniqueID("evidence"), GoalID: goalID, CriterionID: item.ID, ArtifactID: after,
				Kind: "kicad.erc", Result: res, ReportPath: result.EvidencePaths[0], CreatedAt: time.Now().UTC(),
				CriteriaRevision: &revision,
				Provenance: &EvidenceProvenance{
					SchemaVersion:    1,
					Claim:            fmt.Sprintf("ERC 违规数 %d 不超过当前标准允许的 %d", *facts.ViolationCount, itemThreshold),
					Coverage:         item.Kind + "/" + item.ID,
					CheckerID:        facts.CheckerID,
					CheckerVersion:   facts.CheckerVersion,
					SourceLevel:      "tool_check",
					InvalidationRule: evidenceInvalidationRule,
				},
			})
			if res != "pass" {
				out.Unmet = append(out.Unmet, item.ID)
			}
		}
	}

	for _, item := range connectionCriteria {
		e, err := a.verifyConnection(ctx, goalID, g, item, before, revision)
		if err != nil {
			return out, false, err
		}
		out.Evidence = append(out.Evidence, e)
		if e.Result != "pass" {
			out.Unmet = append(out.Unmet, item.ID)
		}
	}

	final, err := a.Artifacts.Digest(ctx, g.ArtifactPath)
	if err != nil {
		return out, false, err
	}
	if final != before {
		return out, false, errors.New("design changed during checks")
	}
	out.Passed = len(out.Unmet) == 0
	current, err := a.State.CommitVerification(ctx, token, out)
	if err != nil {
		return out, false, err
	}
	return out, current, nil
}

// ercThreshold reads a criterion's max_violations; a missing threshold keeps
// the validation default of zero.
func ercThreshold(item Criterion) int {
	var payload struct {
		MaxViolations int `json:"max_violations"`
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return 0
	}
	return payload.MaxViolations
}

// verifyConnection checks the requested sensor connection against fresh
// design facts and builds provenance-backed evidence bound to the artifact
// digest and criteria revision. The check is unverifiable (an error, not a
// pass) when the checker identity is absent from the observation.
func (a *Activities) verifyConnection(ctx context.Context, goalID string, g Goal, item Criterion, digest string, revision int) (Evidence, error) {
	empty := Evidence{}
	design, err := a.Kicad.Call(ctx, CapabilityRequest{ProtocolVersion: 1, OperationID: uniqueID("inspect"), Kind: "inspect_design", GoalID: goalID, ExpectedArtifactID: digest, Payload: requestPayload(g, nil)})
	if err != nil {
		return empty, err
	}
	if design.ActualArtifactID != digest {
		return empty, errors.New("design changed during criteria verification")
	}
	var facts struct {
		ConnectionPresent bool   `json:"sensor.connection_present"`
		CheckerID         string `json:"connection_checker_id"`
		CheckerVersion    string `json:"connection_checker_version"`
	}
	if err = json.Unmarshal(design.Postcondition, &facts); err != nil {
		return empty, err
	}
	if facts.CheckerID == "" || facts.CheckerVersion == "" {
		return empty, errors.New("connection checker identity missing from observation")
	}
	result := "fail"
	if design.Status == "observed" && facts.ConnectionPresent {
		result = "pass"
	}
	return Evidence{
		ID: uniqueID("evidence"), GoalID: goalID, CriterionID: item.ID, ArtifactID: digest,
		Kind: "sensor.connection_present", Result: result, CreatedAt: time.Now().UTC(),
		CriteriaRevision: &revision,
		Provenance: &EvidenceProvenance{
			SchemaVersion:    1,
			Claim:            "传感器连线 RT1.2-J1.2 存在",
			Coverage:         item.Kind + "/" + item.ID,
			CheckerID:        facts.CheckerID,
			CheckerVersion:   facts.CheckerVersion,
			SourceLevel:      "tool_check",
			InvalidationRule: evidenceInvalidationRule,
		},
	}, nil
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
	if snap.Goal.Status == GoalPendingReverification {
		// A confirmed criteria change is reverified independently before any
		// model decision: pass ends the round; only a failure enters the repair
		// flow below.
		var result VerificationResult
		var current bool
		for attempt := 0; attempt < 2; attempt++ {
			result, current, err = a.VerifyGoal(ctx, goalID)
			if err != nil {
				// Verification unavailable: keep the goal pending and retry on
				// the next wake instead of deciding against stale facts.
				return finish(false, nil)
			}
			if current {
				break
			}
			// Criteria changed again mid-check: the round was kept as history;
			// retry once with a fresh token.
		}
		if !current {
			// Still stale after a retry: the newest criteria_updated event will
			// drive the next round; leave the goal pending.
			return finish(false, nil)
		}
		if result.Passed {
			return finish(true, nil)
		}
		snap, err = a.State.GetGoalSnapshot(ctx, goalID)
		if err != nil {
			return finish(false, err)
		}
	}
	valid := make([]Evidence, 0)
	for _, e := range snap.Evidence {
		if EvidenceCurrent(snap.Goal, observed.ArtifactID, e) {
			valid = append(valid, e)
		}
	}
	token := VerificationToken{GoalID: goalID, CriteriaRevision: snap.Goal.CriteriaRevision, ArtifactID: observed.ArtifactID}
	setStatus := func(status GoalStatus, reason string) {
		_, _ = a.State.UpdateStatusForToken(ctx, token, status, reason)
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
		setStatus(GoalNeedsHuman, "model unavailable: "+err.Error())
		return finish(false, nil)
	}
	decisionRevision := token.CriteriaRevision
	decision := Decision{ID: uniqueID("decision"), AgentID: snap.Agent.ID, ObservationID: observed.ID, Proposal: proposal, ModelRunID: modelRunID, ModelCallID: modelCallID, CreatedAt: time.Now().UTC(), CriteriaRevision: &decisionRevision}
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
	// The model answered against the round's token; a criteria or artifact
	// change meanwhile makes its proposal stale, so it must not drive actions
	// or status changes on the new revision.
	latest, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return finish(false, err)
	}
	if latest.Goal.CriteriaRevision != token.CriteriaRevision || latest.Goal.CurrentArtifactID != token.ArtifactID {
		return finish(false, nil)
	}
	switch proposal.Kind {
	case "execute_capability":
		if err = a.Policy.Check(snap.Goal, observed, proposal); err != nil {
			setStatus(GoalNeedsHuman, "policy rejected: "+err.Error())
			return finish(false, nil)
		}
		action, err := a.Executor.ExecuteOrReconcile(ctx, decision.ID)
		if err != nil {
			setStatus(GoalWaiting, "action outcome unknown: "+err.Error())
			return finish(false, nil)
		}
		if action.Status != "applied" {
			setStatus(GoalNeedsHuman, action.Reason)
			return finish(false, nil)
		}
		if _, err = a.ObserveGoal(ctx, goalID, eventID); err != nil {
			return finish(false, err)
		}
		result, current, err := a.VerifyGoal(ctx, goalID)
		if err != nil {
			setStatus(GoalWaiting, "verification failed: "+err.Error())
			return finish(false, nil)
		}
		done, err := a.finishIfVerified(ctx, goalID, result, current)
		return finish(done, err)
	case "open_computer":
		if err = a.Policy.Check(snap.Goal, observed, proposal); err != nil {
			setStatus(GoalNeedsHuman, "policy rejected: "+err.Error())
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
		setStatus(GoalWaiting, "computer session opened")
	case "observe":
		result, current, err := a.VerifyGoal(ctx, goalID)
		if err != nil {
			setStatus(GoalWaiting, "verification unavailable: "+err.Error())
			return finish(false, nil)
		}
		done, err := a.finishIfVerified(ctx, goalID, result, current)
		return finish(done, err)
	case "wait":
		setStatus(GoalWaiting, proposal.Reason)
	case "ask_human":
		if _, err = a.State.InsertMessage(ctx, SessionMessage{ID: uniqueID("msg"), GoalID: goalID, Role: MessageRoleAgent, Kind: MessageKindQuestion, Text: proposal.Reason}); err != nil {
			return finish(false, err)
		}
		setStatus(GoalNeedsHuman, proposal.Reason)
	default:
		setStatus(GoalNeedsHuman, "unsupported model action: "+proposal.Kind)
	}
	return finish(false, nil)
}

// finishIfVerified reports workflow completion only when the committed round
// was current and passed; a stale round changes nothing and leaves the next
// wake to decide.
func (a *Activities) finishIfVerified(ctx context.Context, goalID string, result VerificationResult, current bool) (bool, error) {
	if !current {
		return false, nil
	}
	if result.Passed {
		return true, nil
	}
	reason := "acceptance criteria not yet satisfied"
	if len(result.Unmet) > 0 {
		reason = "unmet criteria: " + strings.Join(result.Unmet, ", ")
	}
	latest, err := a.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return false, err
	}
	token := VerificationToken{GoalID: goalID, CriteriaRevision: latest.Goal.CriteriaRevision, ArtifactID: latest.Goal.CurrentArtifactID}
	_, err = a.State.UpdateStatusForToken(ctx, token, GoalNeedsHuman, reason)
	return false, err
}

// WaitingForHuman reports whether an agent question is still unanswered; the
// workflow uses it to stop timer-driven re-evaluation while a human reply is
// pending.
func (a *Activities) WaitingForHuman(ctx context.Context, goalID string) (bool, error) {
	_, found, err := a.State.UnansweredQuestion(ctx, goalID)
	return found, err
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
