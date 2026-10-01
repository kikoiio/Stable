package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/core"
	"stable/internal/dependency"
	"stable/internal/report"
	"stable/internal/store"
)

type fixedCollector struct{ snapshots []core.DependencySnapshot }

func (c fixedCollector) Collect(context.Context, core.Goal) ([]core.DependencySnapshot, error) {
	return c.snapshots, nil
}

func dependencySet(ercDigest string) []core.DependencySnapshot {
	return []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, Sources: []core.DependencySource{{Kind: "project", Identity: "sensor.kicad_pro", Digest: ercDigest, State: "available"}}, CheckerID: "kicad-cli-erc", CheckerVersion: "9.0.0", Fingerprint: ercDigest, Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, Sources: []core.DependencySource{{Kind: "checker", Identity: "stable", Digest: "stable", State: "available"}}, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection-v1", Available: true},
	}
}

func TestStatusSnapshotRefreshesAndPersistsBeforeRead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	design := filepath.Join(root, "sensor.kicad_sch")
	if err := os.WriteFile(design, []byte("design"), 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("design"))
	digest := hex.EncodeToString(hash[:])
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.CreateGoal(ctx, core.Goal{ID: "g", ArtifactPath: design, AllowedRoot: root, CurrentArtifactID: digest, CriteriaRevision: 1, Status: core.GoalVerified, Criteria: []core.Criterion{{ID: "erc", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"max_violations":0}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileDependencies(ctx, "g", dependencySet("before")); err != nil {
		t.Fatal(err)
	}
	refresher := &dependency.Refresher{State: s, Collector: fixedCollector{dependencySet("after")}, Wake: func(context.Context, string, string) error { return errors.New("Temporal unavailable") }}
	snapshot, err := refreshedSnapshot(ctx, s, refresher, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.DependencyRevision != 1 {
		t.Fatalf("status returned before dependency change persisted: %+v", snapshot.Goal)
	}
	events, err := s.UnprocessedEvents(ctx)
	if err != nil || len(events) != 1 || events[0].Status != "pending" {
		t.Fatalf("failed wake must leave durable event: %+v err=%v", events, err)
	}
	// The committed state survives closing and reopening the database.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reloaded, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || reloaded.Goal.Status != core.GoalPendingReverification || reloaded.Goal.DependencyRevision != 1 {
		t.Fatalf("persisted pending state lost: %+v err=%v", reloaded.Goal, err)
	}
}

func TestExportRefreshesDependenciesBeforeWritingDelivery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	design := filepath.Join(root, "sensor.kicad_sch")
	if err := os.WriteFile(design, []byte("design"), 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("design"))
	digest := hex.EncodeToString(hash[:])
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateGoal(ctx, core.Goal{ID: "export-goal", ArtifactPath: design, AllowedRoot: root, CurrentArtifactID: digest, CriteriaRevision: 1, Status: core.GoalVerified}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileDependencies(ctx, "export-goal", dependencySet("before")); err != nil {
		t.Fatal(err)
	}
	refresher := &dependency.Refresher{State: s, Collector: fixedCollector{dependencySet("after")}}
	outDir := filepath.Join(root, "delivery")
	status, err := refreshedExport(ctx, s, refresher, "export-goal", outDir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Verified || status.Snapshot.Goal.Status != core.GoalPendingReverification || status.Snapshot.Goal.DependencyRevision != 1 {
		t.Fatalf("export used pre-refresh state: %+v", status)
	}
	var saved report.Status
	data, err := os.ReadFile(filepath.Join(outDir, "delivery.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Verified || saved.Snapshot.Goal.DependencyRevision != 1 {
		t.Fatalf("delivery did not contain refreshed state: %+v err=%v", saved, err)
	}
}
