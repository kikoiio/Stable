package dependency

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"stable/internal/core"
	"stable/internal/store"
)

type snapshotCollector struct{ snapshots []core.DependencySnapshot }

func (c *snapshotCollector) Collect(context.Context, core.Goal) ([]core.DependencySnapshot, error) {
	return c.snapshots, nil
}

func newDependencyTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateGoal(context.Background(), core.Goal{ID: "g", Objective: "test", AllowedRoot: t.TempDir()})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func dependencyPair(ercFingerprint string) []core.DependencySnapshot {
	return []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.8", Fingerprint: ercFingerprint, Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
}

func TestRefreshWakeFailureLeavesDurablePendingState(t *testing.T) {
	ctx := context.Background()
	state := newDependencyTestStore(t)
	collector := &snapshotCollector{snapshots: dependencyPair("erc-v1")}
	wakeCalls := 0
	refresher := &Refresher{State: state, Collector: collector, Wake: func(context.Context, string, string) error {
		wakeCalls++
		return errors.New("temporal unavailable")
	}}
	baseline, err := refresher.Refresh(ctx, "g")
	if err != nil || baseline.DependencyRevision != 0 || baseline.Event != nil || wakeCalls != 0 {
		t.Fatalf("baseline refresh: %+v wakeCalls=%d err=%v", baseline, wakeCalls, err)
	}
	collector.snapshots = dependencyPair("erc-v2")
	result, err := refresher.Refresh(ctx, "g")
	if err != nil || result.WakeError == "" || result.Event == nil || result.Event.Status != "pending" || wakeCalls != 1 {
		t.Fatalf("failed wake result: %+v wakeCalls=%d err=%v", result, wakeCalls, err)
	}
	if result.Snapshot.Goal.Status != core.GoalPendingReverification || result.Snapshot.Goal.DependencyRevision != 1 || len(result.Snapshot.Events) != 1 || result.Snapshot.Events[0].Status != "pending" {
		t.Fatalf("wake failure lost persisted state: %+v", result.Snapshot)
	}
	// Repeating an identical query does not make a second change event.
	result, err = refresher.Refresh(ctx, "g")
	if err != nil || result.Event != nil || wakeCalls != 1 || result.DependencyRevision != 1 {
		t.Fatalf("same snapshot was not idempotent: %+v wakeCalls=%d err=%v", result, wakeCalls, err)
	}
}

func TestRefreshSignalsDurableChange(t *testing.T) {
	ctx := context.Background()
	state := newDependencyTestStore(t)
	collector := &snapshotCollector{snapshots: dependencyPair("erc-v1")}
	signaledID := ""
	refresher := &Refresher{State: state, Collector: collector, Wake: func(_ context.Context, goalID, eventID string) error {
		if goalID != "g" {
			t.Fatalf("wake goal = %q", goalID)
		}
		signaledID = eventID
		return nil
	}}
	if _, err := refresher.Refresh(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	collector.snapshots = dependencyPair("erc-v2")
	result, err := refresher.Refresh(ctx, "g")
	if err != nil || signaledID == "" || result.Event == nil || result.Event.Status != "signaled" || result.Snapshot.Events[0].Status != "signaled" {
		t.Fatalf("changed snapshot was not signaled: %+v event=%q err=%v", result, signaledID, err)
	}
}

func TestRefreshCollectorSystemFailureReturnsError(t *testing.T) {
	refresher := &Refresher{State: newDependencyTestStore(t), Collector: collectorFailure{}}
	if _, err := refresher.Refresh(context.Background(), "g"); err == nil {
		t.Fatal("collector system failure was turned into success")
	}
}

type collectorFailure struct{}

func (collectorFailure) Collect(context.Context, core.Goal) ([]core.DependencySnapshot, error) {
	return nil, errors.New("bridge failed")
}
