package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
	d := core.Decision{ID: "d-1", AgentID: "agent-goal-1", ObservationID: "ob-1", Proposal: core.ProposedAction{Kind: "wait", Reason: "test"}}
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
