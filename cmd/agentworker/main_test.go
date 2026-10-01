package main

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"stable/internal/core"
	"stable/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertEvent(t *testing.T, ctx context.Context, s *store.Store, id string) {
	t.Helper()
	_, _, err := s.InsertEventIfAbsent(ctx, core.Event{ID: id, GoalID: "goal-1", Kind: core.EventKindCriteriaUpdate})
	if err != nil {
		t.Fatal(err)
	}
}

// The replay unit feeds the mock waker pending and signaled events, never
// processed ones; delivered events advance to signaled.
func TestReplayDeliversPendingAndSignaled(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.CreateGoal(ctx, core.Goal{ID: "goal-1", Objective: "x", AllowedRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	insertEvent(t, ctx, s, "evt-pending")
	insertEvent(t, ctx, s, "evt-signaled")
	if err := s.SetEventStatus(ctx, "evt-signaled", "signaled"); err != nil {
		t.Fatal(err)
	}
	insertEvent(t, ctx, s, "evt-processed")
	if err := s.SetEventStatus(ctx, "evt-processed", "signaled"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEventStatus(ctx, "evt-processed", "processed"); err != nil {
		t.Fatal(err)
	}

	events, err := s.UnprocessedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var woke []string
	delivered, failed := replayEvents(ctx, events,
		func(_ context.Context, goalID, eventID string) error {
			if goalID != "goal-1" {
				t.Errorf("goal ID: %q", goalID)
			}
			woke = append(woke, eventID)
			return nil
		},
		func(_ context.Context, id string) error { return s.SetEventStatus(ctx, id, "signaled") },
		func(string, ...any) {})
	if delivered != 2 || failed != 0 {
		t.Fatalf("delivered=%d failed=%d", delivered, failed)
	}
	sort.Strings(woke)
	if len(woke) != 2 || woke[0] != "evt-pending" || woke[1] != "evt-signaled" {
		t.Fatalf("woken events: %v", woke)
	}
	// evt-processed must never reach the waker and stays processed.
	for _, id := range woke {
		if id == "evt-processed" {
			t.Fatal("processed event re-delivered")
		}
	}
	remaining, err := s.UnprocessedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("events after replay: %+v", remaining)
	}
	for _, e := range remaining {
		if e.Status != "signaled" {
			t.Fatalf("event %s status %q after delivery", e.ID, e.Status)
		}
	}
}

// A failed delivery only logs: the event keeps its state and is picked up by
// the next restart's replay.
func TestReplayFailureKeepsEventForRetry(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if _, err := s.CreateGoal(ctx, core.Goal{ID: "goal-1", Objective: "x", AllowedRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	insertEvent(t, ctx, s, "evt-ok")
	insertEvent(t, ctx, s, "evt-fail")

	events, err := s.UnprocessedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delivered, failed := replayEvents(ctx, events,
		func(_ context.Context, _, eventID string) error {
			if eventID == "evt-fail" {
				return errors.New("temporal down")
			}
			return nil
		},
		func(_ context.Context, id string) error { return s.SetEventStatus(ctx, id, "signaled") },
		func(string, ...any) {})
	if delivered != 1 || failed != 1 {
		t.Fatalf("delivered=%d failed=%d", delivered, failed)
	}
	remaining, err := s.UnprocessedEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, e := range remaining {
		status[e.ID] = e.Status
	}
	if status["evt-fail"] != "pending" || status["evt-ok"] != "signaled" {
		t.Fatalf("states after failed replay: %v", status)
	}
	// Retry after the outage: the failed event is delivered on the next pass.
	var woke []string
	delivered, failed = replayEvents(ctx, remaining,
		func(_ context.Context, _, eventID string) error {
			woke = append(woke, eventID)
			return nil
		},
		func(_ context.Context, id string) error { return s.SetEventStatus(ctx, id, "signaled") },
		func(string, ...any) {})
	if delivered != 2 || failed != 0 || len(woke) != 2 {
		t.Fatalf("retry: delivered=%d failed=%d woke=%v", delivered, failed, woke)
	}
	if remaining, err = s.UnprocessedEvents(ctx); err != nil || len(remaining) != 2 {
		t.Fatalf("events after retry: %+v %v", remaining, err)
	}
}
