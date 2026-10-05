package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/store"
)

// acceptanceFixture builds a database holding one goal whose candidate repair
// was decided but interrupted before the apply journal reached finalized.
func acceptanceFixture(t *testing.T) (dbPath string, formal, candRoot string, d candidate.AcceptanceDecision) {
	t.Helper()
	root := t.TempDir()
	dbPath = filepath.Join(root, "state.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	formal = filepath.Join(root, "formal")
	candRoot = filepath.Join(root, "candidate")
	for _, p := range []string{formal, candRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGoal(ctx, core.Goal{ID: "goal-1", Objective: "sensor", AllowedRoot: formal, AllowedCapabilities: []string{"kicad.repair_connection"}}); err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(candRoot)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.Candidate{ID: "cand", FormalRoot: formal, CandidateRoot: candRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err := s.SaveCandidate(ctx, store.CandidateRecord{Candidate: c, ActionID: "act", GoalID: "goal-1"}); err != nil {
		t.Fatal(err)
	}
	d = candidate.AcceptanceDecision{ID: "decision", UserID: "user", CandidateID: c.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err := s.SaveAcceptanceDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	return dbPath, formal, candRoot, d
}

func TestReconcileBeforeDispatch(t *testing.T) {
	dbPath, formal, candRoot, d := acceptanceFixture(t)
	if err := ReconcileBeforeDispatch(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal project after reconcile=%q err=%v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(candRoot, "board"))
	if err != nil || string(data) != "old" {
		t.Fatalf("candidate copy after reconcile=%q err=%v", data, err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	receipt, ok, err := s.FindAcceptanceReceipt(ctx, d.ID)
	if err != nil || !ok {
		t.Fatalf("receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("goal not awaiting independent reverification: %+v", snapshot.Goal)
	}
	// A repeated startup reconcile must not apply the decision a second time.
	if err := ReconcileBeforeDispatch(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
}

func TestReconcileBeforeDispatchBlocksConflict(t *testing.T) {
	dbPath, formal, _, d := acceptanceFixture(t)
	// The formal project changed outside the accepted preview: the interrupted
	// acceptance cannot be retried and must be quarantined, not applied.
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileBeforeDispatch(context.Background(), dbPath); err == nil {
		t.Fatal("conflicting formal project was not blocked")
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "external" {
		t.Fatalf("formal project changed despite conflict: %q %v", data, err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var phase string
	if err := s.DB().QueryRow(`SELECT phase FROM acceptance_apply_journal WHERE decision_id=?`, d.ID).Scan(&phase); err != nil || phase != "blocked" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
	if _, ok, err := s.FindAcceptanceReceipt(context.Background(), d.ID); err != nil || ok {
		t.Fatalf("blocked acceptance produced a receipt: ok=%v err=%v", ok, err)
	}
	// Once quarantined, a later startup proceeds: the blocked entry stays
	// visible in the journal but no longer fails the reconcile pass.
	if err := ReconcileBeforeDispatch(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}
}

// The runtime tool whitelist covers the five file tools, the controlled
// command executor and the six M06 interaction/task tools, sorted by name
// without duplicates.
func TestRuntimeToolSchemas(t *testing.T) {
	schemas := runtimeToolSchemas()
	want := []string{
		"ask_user", "command", "edit_file", "exit_plan_mode", "glob", "grep",
		"load_skill", "read_file", "task_create", "task_get", "task_list",
		"task_update", "write_file",
	}
	if len(schemas) != len(want) {
		t.Fatalf("schema count = %d, want %d", len(schemas), len(want))
	}
	seen := map[string]bool{}
	for i, schema := range schemas {
		if schema.Name != want[i] {
			t.Fatalf("schemas[%d] = %q, want %q", i, schema.Name, want[i])
		}
		if seen[schema.Name] {
			t.Fatalf("duplicate schema name %q", schema.Name)
		}
		seen[schema.Name] = true
		if schema.Description == "" || schema.InputSchema == nil {
			t.Fatalf("schema %q is missing description or input schema", schema.Name)
		}
	}
}
