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
	d := core.Decision{ID: "d-1", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "wait", Reason: "test"}, CriteriaRevision: &rev}
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
	if snap.Decisions[0].CriteriaRevision == nil || *snap.Decisions[0].CriteriaRevision != 0 {
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
	d := core.Decision{ID: "d-old", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "repair"}, CriteriaRevision: &rev0}
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
	d = core.Decision{ID: "d-new", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "repair"}, CriteriaRevision: &rev1}
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
		if dec.ID == "d-legacy" && dec.CriteriaRevision != nil {
			t.Fatalf("legacy decision revision backfilled: %+v", dec)
		}
	}
}

func verificationEvidence(id, criterion string, rev int) core.Evidence {
	return core.Evidence{
		ID: id, GoalID: "goal-1", CriterionID: criterion, ArtifactID: "sha-a", Kind: "kicad.erc",
		Result: "pass", ReportPath: "/tmp/" + id + ".json", CriteriaRevision: &rev,
		Provenance: &core.EvidenceProvenance{
			SchemaVersion: 1, Claim: "checked", Coverage: criterion, CheckerID: "kicad-cli-erc",
			CheckerVersion: "8.0.7", SourceLevel: "tool_check", InvalidationRule: "criteria or artifact change",
		},
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
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a"}

	// All criteria pass with a matching token: goal becomes verified.
	current, err := s.CommitVerification(ctx, token, core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("ev-1", "erc", 0)}, Passed: true})
	if err != nil || !current {
		t.Fatalf("commit: %v %v", current, err)
	}
	snap, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil || snap.Goal.Status != core.GoalVerified {
		t.Fatalf("goal not verified: %+v", snap.Goal)
	}
	if len(snap.Evidence) != 1 || snap.Evidence[0].InvalidatedReason != "" || snap.Evidence[0].CriteriaRevision == nil {
		t.Fatalf("evidence stored wrong: %+v", snap.Evidence)
	}

	// Matching token with unmet criteria: goal goes active with the unmet list.
	current, err = s.CommitVerification(ctx, token, core.VerificationResult{
		Evidence: []core.Evidence{verificationEvidence("ev-2", "erc", 0), func() core.Evidence { e := verificationEvidence("ev-3", "conn", 0); e.Result = "fail"; return e }()},
		Passed:   false,
		Unmet:    []string{"conn"},
	})
	if err != nil || !current {
		t.Fatalf("commit unmet: %v %v", current, err)
	}
	snap, _ = s.GetGoalSnapshot(ctx, "goal-1")
	if snap.Goal.Status != core.GoalActive || !strings.Contains(snap.Goal.Reason, "conn") {
		t.Fatalf("goal not active with unmet: %+v", snap.Goal)
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
	if err := s.SetCurrentArtifact(ctx, "goal-1", "sha-a"); err != nil {
		t.Fatal(err)
	}
	token := core.VerificationToken{GoalID: "goal-1", CriteriaRevision: 0, ArtifactID: "sha-a"}
	round := core.VerificationResult{Evidence: []core.Evidence{verificationEvidence("ev-1", "erc", 0)}, Passed: true}
	// A crash between commit and response makes the caller retry the same
	// round: INSERT OR REPLACE keeps exactly one row with stable fields.
	if _, err := s.CommitVerification(ctx, token, round); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
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
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
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
