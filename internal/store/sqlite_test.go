package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/core"
)

func newGoalStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, err = s.CreateGoal(context.Background(), core.Goal{ID: "goal-1", Objective: "sensor", AllowedRoot: t.TempDir(), AllowedCapabilities: []string{"repair"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestOpenV4(t *testing.T) {
	s, _ := newGoalStore(t)
	var version int
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("database version = %d, err = %v", version, err)
	}
	for table, column := range map[string]string{"goals": "dependency_revision", "decisions": "dependency_revision"} {
		if !hasColumn(s.DB(), table, column) {
			t.Fatalf("%s.%s missing from v4 schema", table, column)
		}
	}
	var table string
	if err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='goal_dependencies'`).Scan(&table); err != nil {
		t.Fatalf("goal_dependencies missing from v4 schema: %v", err)
	}
}

func TestGoalSourceSessionRoundtrip(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	if _, err := s.CreateGoal(ctx, core.Goal{ID: "new-goal", Objective: "source attribution", AllowedRoot: t.TempDir(), SourceSessionID: "session-abc"}); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetGoalSnapshot(ctx, "new-goal")
	if err != nil || g.Goal.SourceSessionID != "session-abc" {
		t.Fatalf("goal source: %+v %v", g, err)
	}
	legacy, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || legacy.Goal.SourceSessionID != "" {
		t.Fatalf("legacy goal source: %+v %v", legacy, err)
	}
}

func TestMigrateV4(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateGoal(ctx, core.Goal{ID: "v3-verified", Objective: "legacy verified", AllowedRoot: t.TempDir(), Criteria: []core.Criterion{{ID: "erc", Kind: "kicad.erc_clean", Payload: json.RawMessage(`{"max_violations":0}`)}}, Status: core.GoalVerified, CurrentArtifactID: "artifact-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`UPDATE goals SET status='verified' WHERE id='v3-verified'; PRAGMA user_version=3;`); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordEvidence(ctx, core.Evidence{ID: "legacy-ev", GoalID: "v3-verified", CriterionID: "erc", ArtifactID: "artifact-old", Kind: "kicad.erc", Result: "pass", ReportPath: "old-report.json"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, err := s.GetGoalSnapshot(ctx, "v3-verified")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.Reason == "" {
		t.Fatalf("legacy verified goal was not invalidated: %+v", snapshot.Goal)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].Result != "pass" || snapshot.Evidence[0].Provenance != nil || snapshot.Evidence[0].InvalidatedReason != "" {
		t.Fatalf("legacy evidence was rewritten: %+v", snapshot.Evidence)
	}
	if len(snapshot.Events) != 1 || snapshot.Events[0].ID != "migrate-v4-v3-verified" || snapshot.Events[0].Status != "pending" {
		t.Fatalf("migration wake event: %+v", snapshot.Events)
	}
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("migrated version = %d, err = %v", version, err)
	}
}

func TestGoalDependencySnapshot(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	empty, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || len(empty.Dependencies) != 0 || empty.Goal.DependencyRevision != 0 {
		t.Fatalf("empty dependency snapshot: dependencies=%+v revision=%d err=%v", empty.Dependencies, empty.Goal.DependencyRevision, err)
	}

	connection := core.DependencySnapshot{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "conn-1", Available: true}
	erc := core.DependencySnapshot{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-1", Available: true}
	for _, snapshot := range []core.DependencySnapshot{connection, erc} { // deliberately insert reverse sort order
		data, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err = s.DB().Exec(`INSERT INTO goal_dependencies(goal_id,family,snapshot_json) VALUES(?,?,?)`, "goal-1", snapshot.Family, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB().Exec(`UPDATE goals SET dependency_revision=2 WHERE id='goal-1'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.DependencyRevision != 2 || len(snapshot.Dependencies) != 2 || snapshot.Dependencies[0].Family != core.CheckFamilyERC || snapshot.Dependencies[1].Family != core.CheckFamilyConnection {
		t.Fatalf("dependencies not loaded in stable order: goal=%+v dependencies=%+v", snapshot.Goal, snapshot.Dependencies)
	}
	if snapshot.Dependencies[0].Fingerprint != "erc-1" || snapshot.Dependencies[1].Fingerprint != "conn-1" {
		t.Fatalf("snapshot JSON did not round trip: %+v", snapshot.Dependencies)
	}
}

func TestDependencyBaseline(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	input := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-base", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "connection-base", Available: true},
	}
	result, err := s.ReconcileDependencies(ctx, "goal-1", input)
	if err != nil {
		t.Fatal(err)
	}
	if result.DependencyRevision != 0 || len(result.ChangedFamilies) != 0 || result.Event != nil || len(result.Snapshot.Dependencies) != 2 {
		t.Fatalf("initial baseline incorrectly treated as change: %+v", result)
	}
	if result.Snapshot.Goal.Status != core.GoalActive || len(result.Snapshot.Events) != 0 {
		t.Fatalf("baseline changed goal state or created event: %+v", result.Snapshot)
	}
	result, err = s.ReconcileDependencies(ctx, "goal-1", input)
	if err != nil || result.DependencyRevision != 0 || result.Event != nil || len(result.ChangedFamilies) != 0 {
		t.Fatalf("repeated baseline was not idempotent: %+v err=%v", result, err)
	}
	if _, err = s.ReconcileDependencies(ctx, "goal-1", input[:1]); err == nil {
		t.Fatal("incomplete dependency batch should be rejected")
	}
}

func TestReconcileDependencyChange(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	base := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-v1", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	if _, err := s.ReconcileDependencies(ctx, "goal-1", base); err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []core.Evidence{
		{ID: "ev-erc", GoalID: "goal-1", CriterionID: "erc", Kind: "kicad.erc", Result: "pass", ReportPath: "erc.json"},
		{ID: "ev-connection", GoalID: "goal-1", CriterionID: "connection", Kind: "sensor.connection_present", Result: "pass"},
	} {
		if err := s.RecordEvidence(ctx, evidence); err != nil {
			t.Fatal(err)
		}
	}
	changed := append([]core.DependencySnapshot(nil), base...)
	changed[0].Fingerprint = "erc-v2"
	result, err := s.ReconcileDependencies(ctx, "goal-1", changed)
	if err != nil {
		t.Fatal(err)
	}
	if result.DependencyRevision != 1 || len(result.ChangedFamilies) != 1 || result.ChangedFamilies[0] != core.CheckFamilyERC || result.Event == nil {
		t.Fatalf("unexpected reconciliation result: %+v", result)
	}
	if result.Snapshot.Goal.Status != core.GoalPendingReverification || result.Snapshot.Agent.Status != "waiting" || len(result.Snapshot.Events) != 1 {
		t.Fatalf("goal state and durable event diverged: %+v", result.Snapshot)
	}
	if result.Snapshot.Events[0].ID != "dependency-change-goal-1-1" || result.Snapshot.Events[0].Kind != core.EventKindDependencyChange || result.Snapshot.Events[0].Status != "pending" {
		t.Fatalf("unexpected wake event: %+v", result.Snapshot.Events[0])
	}
	if len(result.Snapshot.Evidence) != 2 {
		t.Fatalf("evidence rows changed: %+v", result.Snapshot.Evidence)
	}
	for _, evidence := range result.Snapshot.Evidence {
		switch evidence.ID {
		case "ev-erc":
			if evidence.Result != "pass" || evidence.InvalidatedReason == "" {
				t.Fatalf("affected evidence did not retain result and gain invalidation: %+v", evidence)
			}
		case "ev-connection":
			if evidence.InvalidatedReason != "" || evidence.Result != "pass" {
				t.Fatalf("unaffected connection evidence was invalidated: %+v", evidence)
			}
		default:
			t.Fatalf("unexpected evidence row: %+v", evidence)
		}
	}
}

func TestReconcileDependencyIdempotence(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	base := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-content-a", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	if _, err := s.ReconcileDependencies(ctx, "goal-1", base); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"erc-history", "connection-current"} {
		kind := "kicad.erc"
		if id == "connection-current" {
			kind = "sensor.connection_present"
		}
		if err := s.RecordEvidence(ctx, core.Evidence{ID: id, GoalID: "goal-1", Kind: kind, Result: "pass", ReportPath: id + ".json"}); err != nil {
			t.Fatal(err)
		}
	}
	missing := append([]core.DependencySnapshot(nil), base...)
	missing[0].Available = false
	missing[0].CheckerVersion = ""
	missing[0].Fingerprint = "erc-missing-required"
	missing[0].Reason = "required project symbol library is missing"
	missing[0].Sources = []core.DependencySource{{Kind: "symbol_library", Identity: "Device", State: "missing_required", Reason: missing[0].Reason}}
	first, err := s.ReconcileDependencies(ctx, "goal-1", missing)
	if err != nil {
		t.Fatal(err)
	}
	if first.DependencyRevision != 1 || len(first.ChangedFamilies) != 1 || first.ChangedFamilies[0] != core.CheckFamilyERC || first.Snapshot.Goal.Status != core.GoalPendingReverification || !strings.Contains(first.Snapshot.Goal.Reason, missing[0].Reason) {
		t.Fatalf("unavailable dependency did not preserve pending state and reason: %+v", first)
	}
	second, err := s.ReconcileDependencies(ctx, "goal-1", missing)
	if err != nil || second.Event != nil || len(second.ChangedFamilies) != 0 || second.DependencyRevision != 1 || len(second.Snapshot.Events) != 1 {
		t.Fatalf("repeated unavailable snapshot was not idempotent: %+v err=%v", second, err)
	}

	recovered := append([]core.DependencySnapshot(nil), base...)
	third, err := s.ReconcileDependencies(ctx, "goal-1", recovered)
	if err != nil {
		t.Fatal(err)
	}
	if third.DependencyRevision != 2 || third.Event == nil || len(third.ChangedFamilies) != 1 || third.ChangedFamilies[0] != core.CheckFamilyERC {
		t.Fatalf("recovery was not recorded as a new dependency change: %+v", third)
	}
	if len(third.Snapshot.Evidence) != 2 {
		t.Fatalf("historical evidence was removed: %+v", third.Snapshot.Evidence)
	}
	for _, evidence := range third.Snapshot.Evidence {
		if evidence.ID == "erc-history" && evidence.InvalidatedReason == "" {
			t.Fatalf("restoring old content erased prior invalidation: %+v", evidence)
		}
	}
	fourth, err := s.ReconcileDependencies(ctx, "goal-1", recovered)
	if err != nil || fourth.Event != nil || fourth.DependencyRevision != 2 || len(fourth.Snapshot.Events) != 2 {
		t.Fatalf("identical recovered snapshot caused duplicate event: %+v err=%v", fourth, err)
	}
}

func TestCommitVerificationDependencyRace(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	base := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-before", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	if _, err := s.ReconcileDependencies(ctx, "goal-1", base); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a", DependencyRevision: 0}
	changed := append([]core.DependencySnapshot(nil), base...)
	changed[0].Fingerprint = "erc-after"
	if _, err := s.ReconcileDependencies(ctx, "goal-1", changed); err != nil {
		t.Fatal(err)
	}
	current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("old-dependency-pass", "erc", 0)}, Passed: true})
	if err != nil || current {
		t.Fatalf("old dependency token unexpectedly committed as current: current=%v err=%v", current, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.DependencyRevision != 1 || len(snapshot.Evidence) != 1 || snapshot.Evidence[0].InvalidatedReason == "" {
		t.Fatalf("dependency race overwrote latest state: goal=%+v evidence=%+v", snapshot.Goal, snapshot.Evidence)
	}
}

func TestGoalRevision(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	a, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Agent.ID != "agent-goal-1" || a.Goal.Revision != 1 {
		t.Fatalf("unexpected snapshot: %+v", a)
	}
	g, err := s.UpdateStatus(ctx, "goal-1", 1, core.GoalWaiting, "await event")
	if err != nil || g.Revision != 2 || g.Status != core.GoalWaiting {
		t.Fatalf("update: %+v %v", g, err)
	}
	_, err = s.UpdateStatus(ctx, "goal-1", 1, core.GoalVerified, "stale")
	if !errors.Is(err, ErrRevision) {
		t.Fatalf("expected ErrRevision, got %v", err)
	}
}

func TestEventPersistenceAndDeduplication(t *testing.T) {
	s, path := newGoalStore(t)
	ctx := context.Background()
	e := core.Event{ID: "event-1", GoalID: "goal-1", Kind: "design_changed"}
	_, inserted, err := s.InsertEventIfAbsent(ctx, e)
	if err != nil || !inserted {
		t.Fatalf("first insert %v %v", inserted, err)
	}
	_, inserted, err = s.InsertEventIfAbsent(ctx, e)
	if err != nil || inserted {
		t.Fatalf("duplicate insert %v %v", inserted, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.PendingEvents(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "event-1" {
		t.Fatalf("pending %+v %v", pending, err)
	}
	if err = s.SetEventStatus(ctx, "event-1", "signaled"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetEventStatus(ctx, "event-1", "processed"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetEventStatus(ctx, "event-1", "signaled"); err == nil {
		t.Fatal("event moved backward")
	}
}

func TestObservationDecisionAndActionReservation(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	o := core.Observation{ID: "ob-1", GoalID: "goal-1", ArtifactID: "sha", Facts: json.RawMessage(`{"ok":true}`)}
	if err := s.RecordObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	rev := 0 // goal-1 starts at criteria revision 0
	depRev := int64(0)
	d := core.Decision{ID: "d-1", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "wait", Reason: "test"}, CriteriaRevision: &rev, DependencyRevision: &depRev}
	if err := s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	a := core.ActionRecord{ID: "action-1", DecisionID: "d-1", ExpectedArtifactID: "sha", DesiredPostcondition: json.RawMessage(`{"connected":true}`)}
	first, err := s.ReserveAction(ctx, a)
	if err != nil || first.Status != "prepared" {
		t.Fatalf("first %+v %v", first, err)
	}
	second, err := s.ReserveAction(ctx, a)
	if err != nil || second.ID != first.ID {
		t.Fatalf("retry %+v %v", second, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || len(snap.Observations) != 1 || len(snap.Decisions) != 1 || len(snap.Actions) != 1 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if snap.Decisions[0].CriteriaRevision == nil || *snap.Decisions[0].CriteriaRevision != 0 || snap.Decisions[0].DependencyRevision == nil || *snap.Decisions[0].DependencyRevision != 0 {
		t.Fatalf("decision revision not persisted: %+v", snap.Decisions[0])
	}
}

func TestStaleDecisionCannotReserveAction(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	if err := s.RecordObservation(ctx, core.Observation{ID: "ob-1", GoalID: "goal-1", ArtifactID: "sha", Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Legacy decision: nil criteria revision can never reserve an action.
	legacy := core.Decision{ID: "d-legacy", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "wait"}}
	if err := s.RecordDecision(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAction(ctx, core.ActionRecord{ID: "a-legacy", DecisionID: "d-legacy", ExpectedArtifactID: "sha"}); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("legacy decision reserved: %v", err)
	}
	// A decision recorded at revision 0 goes stale once criteria move to v1.
	rev0 := 0
	depRev0 := int64(0)
	d := core.Decision{ID: "d-old", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "repair"}, CriteriaRevision: &rev0, DependencyRevision: &depRev0}
	if err := s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	criteria := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	if _, err := s.UpdateGoalCriteria(ctx, "goal-1", criteria); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAction(ctx, core.ActionRecord{ID: "a-old", DecisionID: "d-old", ExpectedArtifactID: "sha"}); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("outdated decision reserved: %v", err)
	}
	// A decision at the current revision reserves fine.
	rev1 := 1
	d = core.Decision{ID: "d-new", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "repair"}, CriteriaRevision: &rev1, DependencyRevision: &depRev0}
	if err := s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAction(ctx, core.ActionRecord{ID: "a-new", DecisionID: "d-new", ExpectedArtifactID: "sha"}); err != nil {
		t.Fatalf("current decision rejected: %v", err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || len(snap.Actions) != 1 || snap.Actions[0].ID != "a-new" {
		t.Fatalf("stale decisions produced actions: %+v", snap.Actions)
	}
	// Legacy decisions read back with a nil revision, never backfilled.
	for _, dec := range snap.Decisions {
		if dec.ID == "d-legacy" && (dec.CriteriaRevision != nil || dec.DependencyRevision != nil) {
			t.Fatalf("legacy decision revision backfilled: %+v", dec)
		}
	}
}

func TestDependencyRevisionGuards(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	baseline := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.8", Fingerprint: "erc-before", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	if _, err := s.ReconcileDependencies(ctx, "goal-1", baseline); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(ctx, core.Observation{ID: "ob-dep", GoalID: "goal-1", ArtifactID: "sha-a", Facts: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	criteriaRevision := 0
	dependencyRevision := int64(0)
	decision := core.Decision{ID: "decision-old-dep", AgentID: "agent-goal-1", ObservationID: "ob-dep", Proposal: core.ProposedAction{Kind: "execute_capability"}, CriteriaRevision: &criteriaRevision, DependencyRevision: &dependencyRevision}
	if err := s.RecordDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	changed := append([]core.DependencySnapshot(nil), baseline...)
	changed[0].Fingerprint = "erc-after"
	if _, err := s.ReconcileDependencies(ctx, "goal-1", changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAction(ctx, core.ActionRecord{ID: "blocked-dep-action", DecisionID: decision.ID, ExpectedArtifactID: "sha-a"}); !errors.Is(err, ErrStaleDecision) {
		t.Fatalf("old dependency decision reserved action: %v", err)
	}
	current, err := s.UpdateStatusForToken(ctx, core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a", DependencyRevision: 0}, core.GoalVerified, "stale success")
	if err != nil || current {
		t.Fatalf("old dependency token changed status: current=%v err=%v", current, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.DependencyRevision != 1 || len(snapshot.Actions) != 0 {
		t.Fatalf("dependency guard failed: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestReconcileDependencyTransactionRollback(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	base := []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad", CheckerVersion: "9.0.8", Fingerprint: "erc-before", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "connection", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
	if _, err := s.ReconcileDependencies(ctx, "goal-1", base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvidence(ctx, core.Evidence{ID: "rollback-ev", GoalID: "goal-1", CriterionID: "erc", Kind: "kicad.erc", Result: "pass"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_dependency_event BEFORE INSERT ON events WHEN NEW.kind='dependency_changed' BEGIN SELECT RAISE(ABORT,'injected event failure'); END`); err != nil {
		t.Fatal(err)
	}
	changed := append([]core.DependencySnapshot(nil), base...)
	changed[0].Fingerprint = "erc-after"
	if _, err := s.ReconcileDependencies(ctx, "goal-1", changed); err == nil {
		t.Fatal("injected wake event failure did not abort reconciliation")
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.DependencyRevision != 0 || snapshot.Goal.Status != core.GoalActive || len(snapshot.Events) != 0 || len(snapshot.Dependencies) != 2 || snapshot.Dependencies[0].Fingerprint != "erc-before" || snapshot.Evidence[0].InvalidatedReason != "" {
		t.Fatalf("failed transaction left partial dependency state: %+v", snapshot)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER reject_dependency_event`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ReconcileDependencies(ctx, "goal-1", changed); err != nil || result.DependencyRevision != 1 || result.Event == nil {
		t.Fatalf("reconciliation did not recover after rollback: %+v err=%v", result, err)
	}
}

func verificationEvidence(id, criterion string, rev int) core.Evidence {
	family := core.CheckFamilyERC
	kind := "kicad.erc"
	checkerID := "kicad-cli-erc"
	checkerVersion := "9.0.8"
	if criterion == "conn" || criterion == "connection" {
		family = core.CheckFamilyConnection
		kind = "sensor.connection_present"
		checkerID = "sensor-connection-check"
		checkerVersion = "1"
	}
	dependency := core.DependencySnapshot{SchemaVersion: 1, Family: family, CheckerID: checkerID, CheckerVersion: checkerVersion, Fingerprint: string(family) + "-fingerprint", Available: true}
	return core.Evidence{
		ID: id, GoalID: "goal-1", CriterionID: criterion, ArtifactID: "sha-a", Kind: kind,
		Result: "pass", ReportPath: "/tmp/" + id + ".json", CriteriaRevision: &rev,
		Provenance: &core.EvidenceProvenance{
			SchemaVersion: 2, Claim: "checked", Coverage: criterion, CheckerID: checkerID,
			CheckerVersion: checkerVersion, SourceLevel: "tool_check", InvalidationRule: "criteria, artifact or dependency change",
			Family: family, Dependency: &dependency,
		},
	}
}

func seedDependencyBaseline(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.ReconcileDependencies(context.Background(), "goal-1", []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.8", Fingerprint: "kicad.erc-fingerprint", Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "sensor.connection-fingerprint", Available: true},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnprocessedEvents(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	insert := func(id string) {
		t.Helper()
		if _, _, err := s.InsertEventIfAbsent(ctx, core.Event{ID: id, GoalID: "goal-1", Kind: "design_changed"}); err != nil {
			t.Fatal(err)
		}
	}
	insert("ev-pending")
	insert("ev-signaled")
	insert("ev-processed")
	if err := s.SetEventStatus(ctx, "ev-signaled", "signaled"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEventStatus(ctx, "ev-processed", "signaled"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEventStatus(ctx, "ev-processed", "processed"); err != nil {
		t.Fatal(err)
	}
	// Duplicate redelivery attempts collapse onto the same event ID.
	if _, inserted, err := s.InsertEventIfAbsent(ctx, core.Event{ID: "ev-pending", GoalID: "goal-1", Kind: "design_changed"}); err != nil || inserted {
		t.Fatalf("duplicate insert: %v %v", inserted, err)
	}
	got, err := s.UnprocessedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "ev-pending" || got[1].ID != "ev-signaled" {
		t.Fatalf("unprocessed: %+v", got)
	}
	if got[1].Status != "signaled" {
		t.Fatalf("signaled status lost: %+v", got[1])
	}
	pending, err := s.PendingEvents(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "ev-pending" {
		t.Fatalf("pending-only query changed: %+v", pending)
	}
}

func TestCommitVerification(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	criteria := []core.Criterion{
		{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	}
	revision, err := s.UpdateGoalCriteria(ctx, "goal-1", criteria)
	if err != nil {
		t.Fatal(err)
	}
	seedDependencyBaseline(t, s)
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	// A prior current connection result can be reused when this round only
	// checks ERC.
	connection := verificationEvidence("connection-existing", "conn", revision)
	if err := s.RecordEvidence(ctx, connection); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: revision, ArtifactID: "sha-a", DependencyRevision: 0}

	// All criteria pass with a matching token: goal becomes verified.
	current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("ev-1", "erc", revision)}, Passed: true})
	if err != nil || !current {
		t.Fatalf("commit: %v %v", current, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snap.Goal.Status != core.GoalVerified {
		t.Fatalf("goal not verified: %+v", snap.Goal)
	}
	if len(snap.Evidence) != 2 || snap.Evidence[0].InvalidatedReason != "" || snap.Evidence[0].CriteriaRevision == nil {
		t.Fatalf("evidence stored wrong: %+v", snap.Evidence)
	}

	// Matching token with unmet criteria: goal goes active with the unmet list.
	current, err = s.CommitVerification(ctx, token, core.VerificationResult{
		Evidence: []core.Evidence{func() core.Evidence { e := verificationEvidence("ev-2", "erc", revision); e.Result = "fail"; return e }()},
		Passed:   false,
		Unmet:    []string{"conn"},
	})
	if err != nil || !current {
		t.Fatalf("commit unmet: %v %v", current, err)
	}
	snap, _ = s.GetGoalSnapshot(ctx, "goal-1")
	if snap.Goal.Status != core.GoalActive || !strings.Contains(snap.Goal.Reason, "erc") {
		t.Fatalf("goal not active with unmet: %+v", snap.Goal)
	}
}

func TestCommitVerificationRequiresAllCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	s, _ := newGoalStore(t)
	revision, err := s.UpdateGoalCriteria(ctx, "goal-1", []core.Criterion{
		{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedDependencyBaseline(t, s)
	if err = s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: revision, ArtifactID: "sha-a", DependencyRevision: 0}
	if current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("only-erc", "erc", revision)}, Passed: true}); err != nil || !current {
		t.Fatalf("partial evidence commit current=%v err=%v", current, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snapshot.Goal.Status == core.GoalVerified || !strings.Contains(snapshot.Goal.Reason, "conn") {
		t.Fatalf("partial evidence was accepted: goal=%+v err=%v", snapshot.Goal, err)
	}
	if current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("only-connection", "conn", revision)}, Passed: false}); err != nil || !current {
		t.Fatalf("connection evidence commit current=%v err=%v", current, err)
	}
	snapshot, err = s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snapshot.Goal.Status != core.GoalVerified {
		t.Fatalf("all-current evidence was not aggregated: goal=%+v err=%v", snapshot.Goal, err)
	}
}

func TestCommitVerificationStaleToken(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a"}
	// Criteria confirmed to v1 while the round was running.
	criteria := []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}
	if _, err := s.UpdateGoalCriteria(ctx, "goal-1", criteria); err != nil {
		t.Fatal(err)
	}
	current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("ev-stale", "erc", 0)}, Passed: true})
	if err != nil {
		t.Fatal(err)
	}
	if current {
		t.Fatal("stale token reported current")
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status != core.GoalActive || snap.Goal.CriteriaRevision != 1 {
		t.Fatalf("stale result changed goal: %+v", snap.Goal)
	}
	// The round's evidence is preserved as history with an invalidation reason.
	if len(snap.Evidence) != 1 || snap.Evidence[0].InvalidatedReason == "" || snap.Evidence[0].Result != "pass" {
		t.Fatalf("stale evidence not kept as history: %+v", snap.Evidence)
	}

	// Artifact digest moved under the token: also rejected.
	token = core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 1, ArtifactID: "sha-b"}
	current, err = s.CommitVerification(ctx, token, core.VerificationResult{Passed: true})
	if err != nil || current {
		t.Fatalf("artifact mismatch accepted: %v %v", current, err)
	}
	if _, err = s.CommitVerification(ctx, core.VerificationToken{GoalID: "missing"}, core.VerificationResult{}); err == nil {
		t.Fatal("missing goal accepted")
	}
}

func TestCommitVerificationReplay(t *testing.T) {
	s, path := newGoalStore(t)
	ctx := context.Background()
	revision, err := s.UpdateGoalCriteria(ctx, "goal-1", []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}})
	if err != nil {
		t.Fatal(err)
	}
	seedDependencyBaseline(t, s)
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: revision, ArtifactID: "sha-a", DependencyRevision: 0}
	round := core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("ev-1", "erc", revision)}, Passed: true}
	// A crash between commit and response makes the caller retry the same
	// round: INSERT OR REPLACE keeps exactly one row with stable fields.
	if _, err := s.CommitVerification(ctx, token, round); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current, err := s.CommitVerification(ctx, token, round)
	if err != nil || !current {
		t.Fatalf("replay: %v %v", current, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || len(snap.Evidence) != 1 || snap.Goal.Status != core.GoalVerified {
		t.Fatalf("replay duplicated evidence: %+v", snap.Evidence)
	}
}

func TestUpdateStatusForToken(t *testing.T) {
	s, _ := newGoalStore(t)
	ctx := context.Background()
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a"}
	current, err := s.UpdateStatusForToken(ctx, token, core.GoalWaiting, "await event")
	if err != nil || !current {
		t.Fatalf("matching token rejected: %v %v", current, err)
	}
	snap, _ := s.GetGoalSnapshot(ctx, "goal-1")
	if snap.Goal.Status != core.GoalWaiting || snap.Goal.Reason != "await event" {
		t.Fatalf("status not updated: %+v", snap.Goal)
	}

	// Stale token on an active goal: no change at all.
	stale := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-other"}
	current, err = s.UpdateStatusForToken(ctx, stale, core.GoalVerified, "stale win")
	if err != nil || current {
		t.Fatalf("stale token applied: %v %v", current, err)
	}
	snap, _ = s.GetGoalSnapshot(ctx, "goal-1")
	if snap.Goal.Status != core.GoalWaiting || snap.Goal.Reason != "await event" {
		t.Fatalf("stale token overwrote state: %+v", snap.Goal)
	}

	// Stale token while pending reverification: stays pending, reason refreshed.
	if _, err = s.UpdateStatus(ctx, "goal-1", snap.Goal.Revision, core.GoalPendingReverification, "待复核"); err != nil {
		t.Fatal(err)
	}
	// Old model outcomes (wait / help / verified) must never override the
	// pending state: every stale status is rejected the same way.
	for _, status := range []core.GoalStatus{core.GoalWaiting, core.GoalNeedsHuman, core.GoalVerified} {
		current, err = s.UpdateStatusForToken(ctx, stale, status, "旧令牌尝试")
		if err != nil || current {
			t.Fatalf("stale %s applied on pending goal: %v %v", status, current, err)
		}
		snap, _ = s.GetGoalSnapshot(ctx, "goal-1")
		if snap.Goal.Status != core.GoalPendingReverification {
			t.Fatalf("stale %s overrode pending_reverification: %+v", status, snap.Goal)
		}
	}
	if !strings.Contains(snap.Goal.Reason, "旧令牌尝试") {
		t.Fatalf("pending reason not refreshed: %q", snap.Goal.Reason)
	}
	if _, err = s.UpdateStatusForToken(ctx, core.VerificationToken{GoalID: "missing"}, core.GoalActive, ""); err == nil {
		t.Fatal("missing goal accepted")
	}
}

func TestEvidenceProvenanceRoundtrip(t *testing.T) {
	s, path := newGoalStore(t)
	ctx := context.Background()
	var version int
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("fresh database version %d %v", version, err)
	}
	rev := 2
	prov := &core.EvidenceProvenance{
		SchemaVersion: 1, Claim: "ERC violations within threshold", Coverage: "kicad.erc_clean",
		CheckerID: "kicad-cli-erc", CheckerVersion: "8.0.7", SourceLevel: "tool_check",
		InvalidationRule: "criteria change or artifact digest change",
	}
	full := core.Evidence{ID: "ev-full", GoalID: "goal-1", CriterionID: "erc", ArtifactID: "sha", Kind: "kicad.erc", Result: "pass", ReportPath: "/tmp/r.json", CriteriaRevision: &rev, Provenance: prov}
	if err := s.RecordEvidence(ctx, full); err != nil {
		t.Fatal(err)
	}
	legacy := core.Evidence{ID: "ev-legacy", GoalID: "goal-1", CriterionID: "erc", ArtifactID: "sha", Kind: "kicad.erc", Result: "pass", ReportPath: "/tmp/old.json"}
	if err := s.RecordEvidence(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || len(snap.Evidence) != 2 {
		t.Fatalf("snapshot %+v %v", snap.Evidence, err)
	}
	got := snap.Evidence[0]
	if got.ID != "ev-full" {
		got = snap.Evidence[1]
	}
	if got.CriteriaRevision == nil || *got.CriteriaRevision != 2 {
		t.Fatalf("revision lost: %+v", got)
	}
	if got.Provenance == nil || *got.Provenance != *prov {
		t.Fatalf("provenance lost: %+v", got.Provenance)
	}
	var old core.Evidence
	for _, e := range snap.Evidence {
		if e.ID == "ev-legacy" {
			old = e
		}
	}
	// Legacy-shaped rows read back with unknown revision/provenance and are
	// never backfilled to the current revision.
	if old.CriteriaRevision != nil || old.Provenance != nil || old.InvalidatedReason != "" {
		t.Fatalf("legacy row mutated: %+v", old)
	}
	var rawRev any
	if err = s.DB().QueryRow(`SELECT criteria_revision FROM evidence WHERE id='ev-legacy'`).Scan(&rawRev); err != nil {
		t.Fatal(err)
	}
	if rawRev != nil {
		t.Fatalf("legacy revision backfilled in storage: %v", rawRev)
	}
}

func TestEvidenceAndSessionGeneration(t *testing.T) {
	s, path := newGoalStore(t)
	ctx := context.Background()
	e := core.Evidence{ID: "ev-1", GoalID: "goal-1", CriterionID: "erc", ArtifactID: "old", Kind: "check", Result: "pass", ReportPath: "/tmp/report.json"}
	if err := s.RecordEvidence(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateEvidence(ctx, "goal-1", "new"); err != nil {
		t.Fatal(err)
	}
	c := core.ComputerSession{ID: "computer-goal-1", GoalID: "goal-1", Status: "open", Generation: 2}
	if err := s.UpsertSession(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Generation = 1
	if err := s.UpsertSession(ctx, c); err == nil {
		t.Fatal("stale generation accepted")
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snap.Session.Generation != 2 || len(snap.Evidence) != 1 || snap.Evidence[0].Result != "stale" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
}
