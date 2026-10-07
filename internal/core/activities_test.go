package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/artifact"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/llm"
	"stable/internal/policy"
	"stable/internal/sessionlog"
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

type goalRunFunc func(context.Context, agent.ExecutionRequest) (agent.RunOutcome, error)

func (f goalRunFunc) RunGoal(ctx context.Context, request agent.ExecutionRequest) (agent.RunOutcome, error) {
	return f(ctx, request)
}

type executorFunc func(context.Context, string) (core.ActionRecord, error)

func (f executorFunc) ExecuteOrReconcile(ctx context.Context, id string) (core.ActionRecord, error) {
	return f(ctx, id)
}

type fixtureRefresher struct {
	state     *store.Store
	snapshots []core.DependencySnapshot
	refreshes int
	onRefresh func()
}

func (r *fixtureRefresher) Refresh(ctx context.Context, goalID string) (core.DependencyRefresh, error) {
	r.refreshes++
	if r.onRefresh != nil {
		r.onRefresh()
	}
	return r.state.ReconcileDependencies(ctx, goalID, r.snapshots)
}

func fixtureActivities(t *testing.T, source ...bool) (*core.Activities, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	withSource := len(source) == 0 || source[0]
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
	var sourceSessionID string
	if withSource {
		sourceSession, sessionErr := sessionlog.Create(root, "goal source")
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		sourceSessionID = sourceSession.ID
	}
	_, err = s.CreateGoal(ctx, core.Goal{ID: "g", Objective: "test", AllowedRoot: root, ArtifactPath: path, CurrentArtifactID: hash, SourceSessionID: sourceSessionID, AllowedCapabilities: []string{"repair"}, Criteria: []core.Criterion{{ID: "erc", Kind: "kicad.erc_clean", Payload: json.RawMessage(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	dependencies := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.4", Fingerprint: "fixture-erc-v1", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "fixture-connection-v1", Available: true},
	}
	if _, err = s.ReconcileDependencies(ctx, "g", dependencies); err != nil {
		t.Fatal(err)
	}
	kicad := callerFunc(func(_ context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
		current, _ := art.Digest(ctx, path)
		result := core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID, ActualArtifactID: current}
		switch r.Kind {
		case "inspect_design":
			result.Status = "observed"
			result.Postcondition = json.RawMessage(`{"sensor.supported":true,"sensor.connection_present":false,"connection_checker_id":"sensor-connection-check","connection_checker_version":"1"}`)
		case "kicad.run_erc":
			var payload struct {
				ReportPath string `json:"report_path"`
			}
			_ = json.Unmarshal(r.Payload, &payload)
			_ = os.WriteFile(payload.ReportPath, []byte(`{"sheets":[]}`), 0644)
			result.Status = "pass"
			result.EvidencePaths = []string{payload.ReportPath}
			result.Postcondition = json.RawMessage(`{"violation_count":0,"checker_id":"kicad-cli-erc","checker_version":"9.0.4"}`)
		default:
			return result, errors.New("unknown capability")
		}
		return result, nil
	})
	a := &core.Activities{State: s, Artifacts: art, Kicad: kicad, Policy: policy.Policy{}, Refresher: &fixtureRefresher{state: s, snapshots: dependencies}}
	return a, s, path
}

func TestCandidateReadyActionBlocksNewLegacyDecisions(t *testing.T) {
	ctx := context.Background()
	a, state, _ := fixtureActivities(t, false)
	decisionCalls := 0
	a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		decisionCalls++
		return core.ProposedAction{Kind: "wait", Reason: "unexpected new decision"}, nil
	})
	// Seed the action state through the store-facing interfaces used by the
	// activity; a ready candidate must remain the sole user-review handoff.
	snap, err := state.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	revision, dependencyRevision := snap.Goal.CriteriaRevision, snap.Goal.DependencyRevision
	if err = state.RecordObservation(ctx, core.Observation{ID: "ready-observation", GoalID: "g", EventID: "ready-observation-event", ArtifactID: snap.Goal.CurrentArtifactID, Facts: json.RawMessage(`{"design":{},"computer":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if err = state.RecordDecision(ctx, core.Decision{ID: "ready-decision", AgentID: "agent-g", ObservationID: "ready-observation", Proposal: core.ProposedAction{Kind: "execute_capability", Capability: "kicad.repair_connection"}, CriteriaRevision: &revision, DependencyRevision: &dependencyRevision}); err != nil {
		t.Fatal(err)
	}
	if _, err = state.ReserveAction(ctx, core.ActionRecord{ID: "action-ready", DecisionID: "ready-decision", ExpectedArtifactID: snap.Goal.CurrentArtifactID, DesiredPostcondition: json.RawMessage(`{"sensor.connection_present":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err = state.SetActionResult(ctx, "action-ready", "candidate_ready", "candidate-digest", "candidate is ready for user review"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.EvaluateGoal(ctx, "g", "ready-timer"); err != nil {
		t.Fatal(err)
	}
	if decisionCalls != 0 {
		t.Fatal("new decision was created while candidate review was pending")
	}
}

func TestLegacyGoalSkipsConversationRunnerAndReachesDecider(t *testing.T) {
	ctx := context.Background()
	a, state, _ := fixtureActivities(t, false)
	runnerCalled := false
	deciderCalled := false
	a.GoalRunner = goalRunFunc(func(context.Context, agent.ExecutionRequest) (agent.RunOutcome, error) {
		runnerCalled = true
		return agent.RunOutcome{Status: agent.RunCompleted}, nil
	})
	a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		deciderCalled = true
		return core.ProposedAction{Kind: "wait", Reason: "legacy path"}, nil
	})
	if _, err := a.EvaluateGoal(ctx, "g", "legacy-timer"); err != nil {
		t.Fatal(err)
	}
	if runnerCalled {
		t.Fatal("conversation runner was invoked for a source-less legacy goal")
	}
	if !deciderCalled {
		t.Fatal("legacy goal did not reach the stable decider")
	}
	snapshot, err := state.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalWaiting || snapshot.Goal.Reason != "legacy path" {
		t.Fatalf("legacy goal status=%s reason=%q", snapshot.Goal.Status, snapshot.Goal.Reason)
	}
}

func TestGoalRunnerOutcomesKeepVerificationUnderStableControl(t *testing.T) {
	for _, status := range []agent.RunStatus{agent.RunFailed, agent.RunCancelled, agent.RunAwaitingTools, agent.RunCompleted} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			a, state, _ := fixtureActivities(t)
			var got agent.ExecutionRequest
			a.GoalRunner = goalRunFunc(func(_ context.Context, request agent.ExecutionRequest) (agent.RunOutcome, error) {
				got = request
				return agent.RunOutcome{RunID: "run-1", Status: status}, nil
			})
			a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
				return core.ProposedAction{Kind: "wait", Reason: "continue stable verification"}, nil
			})
			if _, err := a.EvaluateGoal(ctx, "g", "work-item"); err != nil {
				t.Fatal(err)
			}
			snapshot, err := state.GetGoalSnapshot(ctx, "g")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Goal.Status == core.GoalVerified {
				t.Fatalf("run outcome %s verified the goal", status)
			}
			if got.Work.Kind != agent.WorkGoal || got.Work.SessionID == "" || got.Work.GoalID != "g" || got.Work.WorkItemID != "work-item" || got.BaselineVersion == "" || len(got.AllowedScope) != 1 {
				t.Fatalf("goal request lost attribution or bounds: %+v", got)
			}
		})
	}
}

type cancellationProvider struct {
	started  chan struct{}
	observed chan struct{}
	once     sync.Once
}

func (p *cancellationProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event, 1)
	errs := make(chan error)
	go func() {
		events <- llm.Event{Kind: llm.TextDelta, Text: "partial"}
		p.once.Do(func() { close(p.started) })
		<-ctx.Done()
		close(p.observed)
		close(events)
		close(errs)
	}()
	return events, errs
}

func TestGoalActivityCancellationThroughSharedSocketNeverVerifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, state, path := fixtureActivities(t)
	root := filepath.Dir(path)
	socket := filepath.Join(root, "conversation.sock")
	provider := &cancellationProvider{started: make(chan struct{}), observed: make(chan struct{})}
	runner := agent.NewRunner(provider, agent.RunnerOptions{MaxRetries: -1})
	serviceCtx, stopService := context.WithCancel(context.Background())
	defer stopService()
	svc, err := conversation.Serve(serviceCtx, conversation.Deps{Store: state, Runner: runner, ProviderName: "fixture", Model: "fixture", ProjectRoot: root, SocketPath: socket})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer svc.Close()
	a.GoalRunner = conversation.GoalSocketClient{Socket: socket}
	activityDone := make(chan error, 1)
	go func() { _, runErr := a.EvaluateGoal(ctx, "g", "goal-cancel-item"); activityDone <- runErr }()
	select {
	case <-provider.started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("goal agent run did not start")
	}
	cancel()
	select {
	case runErr := <-activityDone:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("activity error=%v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled goal activity did not finish")
	}
	select {
	case <-provider.observed:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not observe cancellation")
	}
	snapshot, err := state.GetGoalSnapshot(context.Background(), "g")
	if err != nil || snapshot.Goal.Status == core.GoalVerified {
		t.Fatalf("goal status after cancel=%+v err=%v", snapshot.Goal, err)
	}
	transcript, err := sessionlog.Replay(root, snapshot.Goal.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var partial, terminal bool
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var run sessionlog.RunEvent
		raw, _ := json.Marshal(event.Data)
		if decodeErr := json.Unmarshal(raw, &run); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if run.RunID == "" {
			continue
		}
		if run.Kind == string(agent.EventTextDelta) {
			partial = true
		}
		if run.Kind == string(agent.EventTerminal) {
			var payload struct {
				Status agent.RunStatus `json:"status"`
			}
			terminalRaw, _ := json.Marshal(run.Payload)
			_ = json.Unmarshal(terminalRaw, &payload)
			terminal = payload.Status == agent.RunCancelled
		}
	}
	if !partial || !terminal {
		t.Fatalf("cancelled goal stream did not persist partial/terminal: %+v", transcript.Events)
	}
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
	if _, _, err = a.VerifyGoal(ctx, "g"); err == nil {
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
	result, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current || !result.Passed {
		t.Fatalf("verification %+v current=%v %v", result, current, err)
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

func TestEvaluateRefreshesDependenciesBeforeObservation(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	refresher := a.Refresher.(*fixtureRefresher)
	refresher.snapshots[0].Fingerprint = "fixture-erc-v2"
	refreshed := false
	refresher.onRefresh = func() { refreshed = true }
	original := a.Kicad.(callerFunc)
	a.Kicad = callerFunc(func(ctx context.Context, request core.CapabilityRequest) (core.CapabilityResult, error) {
		if !refreshed {
			t.Fatal("KiCad was called before dependency refresh")
		}
		return original(ctx, request)
	})
	done, err := a.EvaluateGoal(ctx, "g", "timer-refresh")
	if err != nil || !done {
		t.Fatalf("evaluate after refreshed dependency: done=%v err=%v", done, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalVerified || snapshot.Goal.DependencyRevision != 1 {
		t.Fatalf("new dependency result not verified: goal=%+v err=%v", snapshot.Goal, err)
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
			result.Postcondition = json.RawMessage(`{"sensor.supported":true,"sensor.connection_present":` + boolJSON(present) + `,"connection_checker_id":"sensor-connection-check","connection_checker_version":"1"}`)
		case "kicad.run_erc":
			var payload struct {
				ReportPath string `json:"report_path"`
			}
			_ = json.Unmarshal(r.Payload, &payload)
			_ = os.WriteFile(payload.ReportPath, []byte(`{}`), 0644)
			result.Status = "pass"
			result.EvidencePaths = []string{payload.ReportPath}
			result.Postcondition = json.RawMessage(`{"violation_count":0,"checker_id":"kicad-cli-erc","checker_version":"9.0.4"}`)
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
	if _, _, err = a.VerifyGoal(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	state, _ := s.GetGoalSnapshot(ctx, "g")
	if state.Goal.Status == core.GoalVerified {
		t.Fatal("verified despite unmet connection criterion")
	}
	// Connection restored: now verification completes.
	present = true
	if _, _, err = a.VerifyGoal(ctx, "g"); err != nil {
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

func TestVerifyOnlyMissingFamily(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	criteria := []core.Criterion{ercCriteria("erc", 0), ercCriteria("erc-relaxed", 1), connCriterion("conn")}
	if _, err := s.UpdateGoalCriteria(ctx, "g", criteria); err != nil {
		t.Fatal(err)
	}
	kicad := &verifyKicad{path: path, present: true}
	a.Kicad = kicad.caller(ctx)
	first, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current || !first.Passed || kicad.calls != 1 || kicad.inspectCalls != 1 {
		t.Fatalf("initial full verification: result=%+v current=%v calls=%d/%d err=%v", first, current, kicad.calls, kicad.inspectCalls, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	var connectionID string
	for _, evidence := range snapshot.Evidence {
		if evidence.CriterionID == "conn" && core.EvidenceCurrentWithDependencies(snapshot.Goal, snapshot.Goal.CurrentArtifactID, snapshot.Dependencies, evidence) {
			connectionID = evidence.ID
		}
	}
	if connectionID == "" {
		t.Fatalf("no current connection evidence: %+v", snapshot.Evidence)
	}
	ercReports := map[string]string{}
	for _, evidence := range snapshot.Evidence {
		if evidence.Kind == "kicad.erc" && evidence.Provenance != nil && evidence.Provenance.Dependency != nil {
			ercReports[evidence.CriterionID] = evidence.ReportPath + "/" + evidence.Provenance.Dependency.Fingerprint
		}
	}
	if len(ercReports) != 2 || ercReports["erc"] != ercReports["erc-relaxed"] {
		t.Fatalf("ERC criteria did not share one report and frozen dependency snapshot: %+v", ercReports)
	}
	refresher := a.Refresher.(*fixtureRefresher)
	refresher.snapshots[0].Fingerprint = "fixture-erc-v2"
	if _, err = refresher.Refresh(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	kicad.calls, kicad.inspectCalls = 0, 0
	second, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current || !second.Passed || kicad.calls != 1 || kicad.inspectCalls != 0 {
		t.Fatalf("targeted ERC verification: result=%+v current=%v calls=%d/%d err=%v", second, current, kicad.calls, kicad.inspectCalls, err)
	}
	snapshot, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalVerified {
		t.Fatalf("combined current evidence did not verify goal: %+v err=%v", snapshot.Goal, err)
	}
	foundConnection := false
	for _, evidence := range snapshot.Evidence {
		if evidence.ID == connectionID && evidence.InvalidatedReason == "" && core.EvidenceCurrentWithDependencies(snapshot.Goal, snapshot.Goal.CurrentArtifactID, snapshot.Dependencies, evidence) {
			foundConnection = true
		}
	}
	if !foundConnection {
		t.Fatalf("unaffected connection evidence was not reused: %+v", snapshot.Evidence)
	}
}

func TestConnectionCheckerChangeOnlyRechecksConnection(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	if _, err := s.UpdateGoalCriteria(ctx, "g", []core.Criterion{ercCriteria("erc", 0), connCriterion("conn")}); err != nil {
		t.Fatal(err)
	}
	kicad := &verifyKicad{path: path, present: true}
	a.Kicad = kicad.caller(ctx)
	if _, current, err := a.VerifyGoal(ctx, "g"); err != nil || !current {
		t.Fatalf("initial verification: current=%v err=%v", current, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	var ercID, connectionID string
	for _, evidence := range snapshot.Evidence {
		if evidence.CriterionID == "erc" {
			ercID = evidence.ID
		}
		if evidence.CriterionID == "conn" {
			connectionID = evidence.ID
		}
	}
	refresher := a.Refresher.(*fixtureRefresher)
	refresher.snapshots[1].CheckerVersion = "2"
	refresher.snapshots[1].Fingerprint = "fixture-connection-v2"
	if _, err = refresher.Refresh(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	kicad.connectionVersion = "2"
	kicad.calls, kicad.inspectCalls = 0, 0
	result, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current || !result.Passed || kicad.calls != 0 || kicad.inspectCalls != 1 {
		t.Fatalf("connection-only refresh: result=%+v current=%v calls=%d/%d err=%v", result, current, kicad.calls, kicad.inspectCalls, err)
	}
	snapshot, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalVerified {
		t.Fatalf("connection-only result did not combine with ERC: %+v err=%v", snapshot.Goal, err)
	}
	for _, evidence := range snapshot.Evidence {
		if evidence.ID == ercID && evidence.InvalidatedReason != "" {
			t.Fatalf("ERC evidence was invalidated by connection checker update: %+v", evidence)
		}
		if evidence.ID == connectionID && evidence.InvalidatedReason == "" {
			t.Fatalf("old connection evidence was not invalidated: %+v", evidence)
		}
	}
}

func TestDependencyChangesDuringVerification(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	if _, err := s.UpdateGoalCriteria(ctx, "g", []core.Criterion{ercCriteria("erc", 0)}); err != nil {
		t.Fatal(err)
	}
	refresher := a.Refresher.(*fixtureRefresher)
	kicad := &verifyKicad{path: path, present: true}
	kicad.onERC = func() { refresher.snapshots[0].Fingerprint = "fixture-erc-during-check" }
	a.Kicad = kicad.caller(ctx)
	oldResult, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || current {
		t.Fatalf("old dependency result accepted: result=%+v current=%v err=%v", oldResult, current, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.DependencyRevision != 1 || len(snapshot.Evidence) != 1 || snapshot.Evidence[0].InvalidatedReason == "" {
		t.Fatalf("mid-check dependency change not persisted: goal=%+v evidence=%+v err=%v", snapshot.Goal, snapshot.Evidence, err)
	}
	kicad.onERC = nil
	newResult, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current || !newResult.Passed || kicad.calls != 2 {
		t.Fatalf("latest dependency was not rechecked: result=%+v current=%v calls=%d err=%v", newResult, current, kicad.calls, err)
	}
	snapshot, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalVerified {
		t.Fatalf("latest dependency evidence did not verify goal: %+v err=%v", snapshot.Goal, err)
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func testDependency(family core.CheckFamily, checker, version, fingerprint string) core.DependencySnapshot {
	return core.DependencySnapshot{SchemaVersion: 1, Family: family, CheckerID: checker, CheckerVersion: version, Fingerprint: fingerprint, Available: true}
}

func testDependencyPointer(family core.CheckFamily, checker, version, fingerprint string) *core.DependencySnapshot {
	dependency := testDependency(family, checker, version, fingerprint)
	return &dependency
}

func currentEvidenceFixture() core.Evidence {
	rev := 2
	return core.Evidence{
		ID:               "ev-1",
		GoalID:           "g",
		CriterionID:      "erc",
		ArtifactID:       "digest-1",
		Kind:             "kicad.erc",
		Result:           "pass",
		ReportPath:       "report.json",
		CriteriaRevision: &rev,
		Provenance: &core.EvidenceProvenance{
			SchemaVersion:    2,
			Claim:            "ERC violations within threshold",
			Coverage:         "erc",
			CheckerID:        "kicad-cli-erc",
			CheckerVersion:   "9.0.4",
			SourceLevel:      "tool_check",
			InvalidationRule: "criteria or artifact change",
			Family:           core.CheckFamilyERC,
			Dependency:       testDependencyPointer(core.CheckFamilyERC, "kicad-cli-erc", "9.0.4", "erc-dependencies"),
		},
	}
}

func TestCurrentEvidence(t *testing.T) {
	goal := core.Goal{ID: "g", CriteriaRevision: 2, CurrentArtifactID: "digest-1"}
	dependencies := []core.DependencySnapshot{testDependency(core.CheckFamilyERC, "kicad-cli-erc", "9.0.4", "erc-dependencies")}
	current := func(e core.Evidence) bool {
		return core.EvidenceCurrentWithDependencies(goal, "digest-1", dependencies, e)
	}
	if !current(currentEvidenceFixture()) {
		t.Fatal("complete passing evidence not recognized as current")
	}
	cases := map[string]func(*core.Evidence){
		"failed result":           func(e *core.Evidence) { e.Result = "fail" },
		"invalidated":             func(e *core.Evidence) { e.InvalidatedReason = "criteria changed" },
		"legacy nil revision":     func(e *core.Evidence) { e.CriteriaRevision = nil },
		"stale revision":          func(e *core.Evidence) { rev := 1; e.CriteriaRevision = &rev },
		"artifact mismatch":       func(e *core.Evidence) { e.ArtifactID = "digest-0" },
		"legacy nil provenance":   func(e *core.Evidence) { e.Provenance = nil },
		"newer schema unknown":    func(e *core.Evidence) { e.Provenance.SchemaVersion = 3 },
		"empty claim":             func(e *core.Evidence) { e.Provenance.Claim = "" },
		"empty coverage":          func(e *core.Evidence) { e.Provenance.Coverage = "" },
		"empty checker id":        func(e *core.Evidence) { e.Provenance.CheckerID = "" },
		"unknown checker version": func(e *core.Evidence) { e.Provenance.CheckerVersion = "" },
		"empty invalidation rule": func(e *core.Evidence) { e.Provenance.InvalidationRule = "" },
		"screenshot observation":  func(e *core.Evidence) { e.Provenance.SourceLevel = "observation" },
		"unknown source":          func(e *core.Evidence) { e.Provenance.SourceLevel = "unknown" },
	}
	for name, mutate := range cases {
		e := currentEvidenceFixture()
		mutate(&e)
		if current(e) {
			t.Fatalf("%s: evidence must not be current", name)
		}
	}
	if core.EvidenceCurrentWithDependencies(goal, "", dependencies, currentEvidenceFixture()) {
		t.Fatal("empty current artifact must not match")
	}
}

// verifyFixtureKicad returns a controllable kicad caller: ERC reports the
// given violation count and inspect reports the connection state; both carry
// checker identity. onERC runs inside each ERC call (to simulate mid-check
// criteria changes).
type verifyKicad struct {
	path              string
	violations        int
	present           bool
	connectionVersion string
	onERC             func()
	calls             int
	inspectCalls      int
}

func (k *verifyKicad) caller(ctx context.Context) callerFunc {
	return func(_ context.Context, r core.CapabilityRequest) (core.CapabilityResult, error) {
		data, _ := os.ReadFile(k.path)
		_ = data
		result := core.CapabilityResult{ProtocolVersion: 1, OperationID: r.OperationID}
		switch r.Kind {
		case "inspect_design":
			k.inspectCalls++
			version := k.connectionVersion
			if version == "" {
				version = "1"
			}
			h := sha256Of(k.path)
			result.Status = "observed"
			result.ActualArtifactID = h
			result.Postcondition = json.RawMessage(`{"sensor.supported":true,"sensor.connection_present":` + boolJSON(k.present) + `,"connection_checker_id":"sensor-connection-check","connection_checker_version":"` + version + `"}`)
		case "kicad.run_erc":
			k.calls++
			if k.onERC != nil {
				k.onERC()
			}
			var payload struct {
				ReportPath string `json:"report_path"`
			}
			_ = json.Unmarshal(r.Payload, &payload)
			_ = os.WriteFile(payload.ReportPath, []byte(`{"sheets":[]}`), 0644)
			result.ActualArtifactID = sha256Of(k.path)
			if k.violations > 0 {
				result.Status = "fail"
			} else {
				result.Status = "pass"
			}
			result.EvidencePaths = []string{payload.ReportPath}
			post, _ := json.Marshal(map[string]any{"violation_count": k.violations, "checker_id": "kicad-cli-erc", "checker_version": "9.0.4"})
			result.Postcondition = post
		default:
			return result, errors.New("unknown capability")
		}
		return result, nil
	}
}

func sha256Of(path string) string {
	h := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	_, _ = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func ercCriteria(id string, max int) core.Criterion {
	payload, _ := json.Marshal(map[string]int{"max_violations": max})
	return core.Criterion{ID: id, Kind: core.CriterionKindERCClean, Payload: payload}
}

func connCriterion(id string) core.Criterion {
	return core.Criterion{ID: id, Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)}
}

func confirmProposal(t *testing.T, ctx context.Context, s *store.Store, goalID, proposalID string, criteria []core.Criterion) {
	t.Helper()
	if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: proposalID, GoalID: goalID, Status: core.ProposalPending, Criteria: criteria}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmGoalCriteria(ctx, proposalID); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyEvidenceProvenanceAndVersionedReportPaths(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	if _, _, err := a.VerifyGoal(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Evidence) != 1 {
		t.Fatalf("evidence %+v", snap.Evidence)
	}
	first := snap.Evidence[0]
	if first.CriteriaRevision == nil || *first.CriteriaRevision != 0 {
		t.Fatalf("revision %+v", first.CriteriaRevision)
	}
	p := first.Provenance
	if p == nil || p.CheckerID != "kicad-cli-erc" || p.CheckerVersion != "9.0.4" || p.SourceLevel != "tool_check" || p.Claim == "" || p.Coverage == "" || p.InvalidationRule == "" {
		t.Fatalf("provenance %+v", p)
	}
	if !strings.Contains(first.ReportPath, "erc-v0-") {
		t.Fatalf("report path %q lacks criteria version", first.ReportPath)
	}
	// A new criteria version must produce a different report path and
	// evidence bound to that version.
	if _, err = s.UpdateGoalCriteria(ctx, "g", []core.Criterion{ercCriteria("erc", 0)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = a.VerifyGoal(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	snap, err = s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	var second *core.Evidence
	for i, e := range snap.Evidence {
		if e.ID != first.ID {
			second = &snap.Evidence[i]
		}
	}
	if second == nil || second.CriteriaRevision == nil || *second.CriteriaRevision != 1 {
		t.Fatalf("second round evidence %+v", second)
	}
	if second.ReportPath == first.ReportPath || !strings.Contains(second.ReportPath, "erc-v1-") {
		t.Fatalf("report paths %q vs %q", first.ReportPath, second.ReportPath)
	}
	if snap.Goal.Status != core.GoalVerified {
		t.Fatalf("status %q", snap.Goal.Status)
	}
}

func TestVerifyERCPerCriterionThresholds(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path, violations: 1}
	a.Kicad = kicad.caller(ctx)
	if _, err := s.UpdateGoalCriteria(ctx, "g", []core.Criterion{ercCriteria("erc-strict", 0), ercCriteria("erc-lax", 2)}); err != nil {
		t.Fatal(err)
	}
	result, current, err := a.VerifyGoal(ctx, "g")
	if err != nil || !current {
		t.Fatalf("verify %v current=%v", err, current)
	}
	if result.Passed || len(result.Unmet) != 1 || result.Unmet[0] != "erc-strict" {
		t.Fatalf("unmet %+v passed=%v", result.Unmet, result.Passed)
	}
	if kicad.calls != 1 {
		t.Fatalf("ERC ran %d times for %d criteria", kicad.calls, 2)
	}
	byID := map[string]core.Evidence{}
	for _, e := range result.Evidence {
		byID[e.CriterionID] = e
	}
	if byID["erc-strict"].Result != "fail" || byID["erc-lax"].Result != "pass" {
		t.Fatalf("per-criterion results %+v", byID)
	}
	if byID["erc-strict"].ReportPath != byID["erc-lax"].ReportPath {
		t.Fatal("one report should serve both ERC criteria")
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status == core.GoalVerified {
		t.Fatal("verified despite unmet strict criterion")
	}
}

func TestVerifyCommitStaleDuringCheck(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path}
	a.Kicad = kicad.caller(ctx)
	// Criteria change lands mid-check: the round's token is stale at commit.
	kicad.onERC = func() {
		if _, err := s.UpdateGoalCriteria(ctx, "g", []core.Criterion{ercCriteria("erc", 0)}); err != nil {
			t.Error(err)
		}
	}
	result, current, err := a.VerifyGoal(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Fatal("stale round reported current")
	}
	if !result.Passed {
		t.Fatalf("round itself passed checks: %+v", result)
	}
	snap, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status == core.GoalVerified {
		t.Fatal("stale round verified the new criteria version")
	}
	if len(snap.Evidence) != 1 || snap.Evidence[0].InvalidatedReason == "" {
		t.Fatalf("stale evidence not kept as invalidated history: %+v", snap.Evidence)
	}
}

func TestPendingReverificationPassSkipsModel(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path, present: true}
	a.Kicad = kicad.caller(ctx)
	confirmProposal(t, ctx, s, "g", "p1", []core.Criterion{ercCriteria("erc", 0), connCriterion("conn")})
	called := false
	a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		called = true
		return core.ProposedAction{Kind: "wait"}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "criteria-confirm-p1")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if called {
		t.Fatal("model called although the new criteria already pass")
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status != core.GoalVerified {
		t.Fatalf("status %q reason %q", snap.Goal.Status, snap.Goal.Reason)
	}
	for _, e := range snap.Evidence {
		if e.InvalidatedReason != "" {
			t.Fatalf("current evidence invalidated: %+v", e)
		}
	}
}

func TestAcceptedCandidateRequiresIndependentGoalReverification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formalRoot := filepath.Join(root, "project")
	if err := os.Mkdir(formalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(formalRoot, "design.txt")
	if err := os.WriteFile(artifactPath, []byte("original design"), 0644); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(root)
	if err != nil {
		t.Fatal(err)
	}
	artifactID, err := artifacts.Digest(ctx, artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	_, err = state.CreateGoal(ctx, core.Goal{
		ID: "g", Objective: "reverify accepted candidate", AllowedRoot: formalRoot,
		ArtifactPath: artifactPath, CurrentArtifactID: artifactID,
		Criteria: []core.Criterion{ercCriteria("erc", 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	dependencies := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.4", Fingerprint: "fixture-erc-v1", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "fixture-connection-v1", Available: true},
	}
	if _, err = state.ReconcileDependencies(ctx, "g", dependencies); err != nil {
		t.Fatal(err)
	}
	checker := &verifyKicad{path: artifactPath, present: false}
	activities := &core.Activities{State: state, Artifacts: artifacts, Kicad: checker.caller(ctx), Refresher: &fixtureRefresher{state: state, snapshots: dependencies}}
	projectCandidate, err := candidate.CreateCandidate("accepted-reverification-candidate", formalRoot, filepath.Join(filepath.Dir(formalRoot), "candidates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectCandidate.CandidateRoot, filepath.Base(artifactPath)), []byte("accepted candidate design"), 0644); err != nil {
		t.Fatal(err)
	}
	projectCandidate, err = candidate.FreezeCandidate(projectCandidate, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	review, err := candidate.BuildReview(ctx, projectCandidate, nil)
	if err != nil {
		t.Fatal(err)
	}
	projectCandidate.Status = "reviewed"
	if err := state.SaveCandidate(ctx, store.CandidateRecord{Candidate: projectCandidate, ActionID: "accepted-reverification-action", GoalID: "g"}); err != nil {
		t.Fatal(err)
	}
	decision := candidate.AcceptanceDecision{
		ID: "accepted-reverification-decision", UserID: "fixture-user", CandidateID: projectCandidate.ID,
		CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest,
		Mode: candidate.AcceptNormal,
	}
	if _, err := candidate.AcceptCandidate(ctx, projectCandidate, review, decision, "g", "", state, time.Time{}); err != nil {
		t.Fatal(err)
	}
	accepted, err := state.GetGoalSnapshot(ctx, "g")
	if err != nil || accepted.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("acceptance skipped independent reverification: goal=%+v err=%v", accepted.Goal, err)
	}

	activities.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		t.Fatal("passing independent verification unexpectedly called the model")
		return core.ProposedAction{}, nil
	})
	done, err := activities.EvaluateGoal(ctx, "g", "accepted-candidate-reverification")
	if err != nil || !done {
		t.Fatalf("independent goal evaluation done=%v err=%v", done, err)
	}
	verified, err := state.GetGoalSnapshot(ctx, "g")
	if err != nil || verified.Goal.Status != core.GoalVerified {
		t.Fatalf("goal became %q after passing independent check: %v", verified.Goal.Status, err)
	}
	if checker.calls != 1 {
		t.Fatalf("independent checker ran %d times, want once", checker.calls)
	}
}

func TestPendingReverificationFailEntersRepair(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path, present: false}
	a.Kicad = kicad.caller(ctx)
	confirmProposal(t, ctx, s, "g", "p1", []core.Criterion{ercCriteria("erc", 0), connCriterion("conn")})
	called := false
	a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		called = true
		return core.ProposedAction{Kind: "wait", Reason: "retry later"}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "criteria-confirm-p1")
	if err != nil || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !called {
		t.Fatal("failed reverification must enter the model decision flow")
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status == core.GoalVerified || snap.Goal.Status == core.GoalPendingReverification {
		t.Fatalf("status %q", snap.Goal.Status)
	}
	if len(snap.Decisions) != 1 || snap.Decisions[0].CriteriaRevision == nil || *snap.Decisions[0].CriteriaRevision != 1 {
		t.Fatalf("decision %+v", snap.Decisions)
	}
}

func TestCriteriaRaceDuringCheck(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path}
	a.Kicad = kicad.caller(ctx)
	confirmProposal(t, ctx, s, "g", "p1", []core.Criterion{ercCriteria("erc", 0)})
	// The controlled pause: a second criteria change lands mid-check, so the
	// first round's commit is stale and reverification retries once.
	kicad.onERC = func() {
		if kicad.calls == 1 {
			if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "p2", GoalID: "g", Status: core.ProposalPending, Criteria: []core.Criterion{ercCriteria("erc", 0)}}); err != nil {
				t.Error(err)
			}
			if _, err := s.ConfirmGoalCriteria(ctx, "p2"); err != nil {
				t.Error(err)
			}
		}
	}
	done, err := a.EvaluateGoal(ctx, "g", "criteria-confirm-p1")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status != core.GoalVerified || snap.Goal.CriteriaRevision != 2 {
		t.Fatalf("goal %+v", snap.Goal)
	}
	var stale, current int
	for _, e := range snap.Evidence {
		if e.InvalidatedReason != "" {
			stale++
			if e.CriteriaRevision == nil || *e.CriteriaRevision != 1 {
				t.Fatalf("stale evidence %+v", e)
			}
		} else {
			current++
			if e.CriteriaRevision == nil || *e.CriteriaRevision != 2 {
				t.Fatalf("current evidence %+v", e)
			}
		}
	}
	if stale != 1 || current != 1 {
		t.Fatalf("stale=%d current=%d", stale, current)
	}
}

func TestStaleDecisionIgnoredAfterCriteriaChange(t *testing.T) {
	ctx := context.Background()
	a, s, _ := fixtureActivities(t)
	a.Decider = deciderFunc(func(context.Context, core.DecisionContext) (core.ProposedAction, error) {
		// Criteria change lands while the model is answering.
		if _, err := s.InsertProposal(ctx, core.CriteriaProposal{ID: "p1", GoalID: "g", Status: core.ProposalPending, Criteria: []core.Criterion{ercCriteria("erc", 0)}}); err != nil {
			t.Error(err)
		}
		if _, err := s.ConfirmGoalCriteria(ctx, "p1"); err != nil {
			t.Error(err)
		}
		return core.ProposedAction{Kind: "wait", Reason: "stale answer"}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "timer-0")
	if err != nil || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("stale model answer changed status to %q", snap.Goal.Status)
	}
	if len(snap.Decisions) != 1 || snap.Decisions[0].CriteriaRevision == nil || *snap.Decisions[0].CriteriaRevision != 0 {
		t.Fatalf("decision %+v", snap.Decisions)
	}
	if len(snap.Actions) != 0 {
		t.Fatalf("stale decision drove actions %+v", snap.Actions)
	}
}

func TestRepairThenVerifyNewToken(t *testing.T) {
	ctx := context.Background()
	a, s, path := fixtureActivities(t)
	kicad := &verifyKicad{path: path, present: false}
	a.Kicad = kicad.caller(ctx)
	confirmProposal(t, ctx, s, "g", "p1", []core.Criterion{ercCriteria("erc", 0), connCriterion("conn")})
	a.Decider = deciderFunc(func(_ context.Context, d core.DecisionContext) (core.ProposedAction, error) {
		return core.ProposedAction{Kind: "execute_capability", Capability: "repair", Target: path, Parameters: json.RawMessage(`{}`), ExpectedArtifactID: d.Observation.ArtifactID, Reason: "restore connection"}, nil
	})
	a.Executor = executorFunc(func(context.Context, string) (core.ActionRecord, error) {
		// The repair changes the design; verification afterwards runs against a
		// fresh token bound to the new artifact.
		if err := os.WriteFile(path, []byte("fixed"), 0644); err != nil {
			t.Error(err)
		}
		kicad.present = true
		digest := sha256Of(path)
		return core.ActionRecord{ID: "action-1", Status: "applied", ResultArtifactID: digest}, nil
	})
	done, err := a.EvaluateGoal(ctx, "g", "criteria-confirm-p1")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	snap, _ := s.GetGoalSnapshot(ctx, "g")
	if snap.Goal.Status != core.GoalVerified {
		t.Fatalf("status %q reason %q", snap.Goal.Status, snap.Goal.Reason)
	}
	newDigest := sha256Of(path)
	if snap.Goal.CurrentArtifactID != newDigest {
		t.Fatalf("current artifact %q", snap.Goal.CurrentArtifactID)
	}
	for _, e := range snap.Evidence {
		if e.Result == "pass" && e.InvalidatedReason == "" && e.ArtifactID != newDigest {
			t.Fatalf("current evidence bound to old artifact: %+v", e)
		}
	}
}
