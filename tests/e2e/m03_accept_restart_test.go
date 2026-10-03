package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/store"
)

func TestM03AcceptanceInterruptedAfterExchangeReopensSQLite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal, candidateRoot := filepath.Join(root, "formal"), filepath.Join(root, "candidate")
	for _, dir := range []string{formal, candidateRoot} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	_, oldDigest, err := candidate.BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	_, newDigest, err := candidate.BuildManifest(candidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state.db")
	state, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	goalID := "m03-restart-goal"
	if _, err = state.CreateGoal(ctx, coreGoal(goalID, formal)); err != nil {
		t.Fatal(err)
	}
	record := candidate.Candidate{ID: "m03-restart-candidate", FormalRoot: formal, CandidateRoot: candidateRoot, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: record, ActionID: "m03-restart-action", GoalID: goalID}); err != nil {
		t.Fatal(err)
	}
	decision := candidate.AcceptanceDecision{ID: "m03-restart-decision", UserID: "local-user", CandidateID: record.ID, CandidateDigest: newDigest, PreviewDigest: "persisted-preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err = state.SaveAcceptanceDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if err = candidate.ExchangeProjectDir(formal, candidateRoot); err != nil {
		t.Fatal(err)
	}
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}

	state, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err = state.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	if err = state.ReconcileAcceptances(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(data) != "new" {
		t.Fatalf("formal project after restart reconciliation=%q err=%v", data, err)
	}
	receipt, ok, err := state.FindAcceptanceReceipt(ctx, decision.ID)
	if err != nil || !ok || receipt.ID != "receipt-"+decision.ID || receipt.FormalDigest != newDigest {
		t.Fatalf("restart receipt=%+v ok=%v err=%v", receipt, ok, err)
	}
	goal, err := state.GetGoalSnapshot(ctx, goalID)
	if err != nil || goal.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("restart incorrectly marked goal verified: status=%s err=%v", goal.Goal.Status, err)
	}
	var receipts int
	if err = state.DB().QueryRowContext(ctx, `SELECT count(*) FROM acceptance_receipts WHERE decision_id=?`, decision.ID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("restart receipt count=%d err=%v", receipts, err)
	}
}

func coreGoal(id, root string) core.Goal {
	return core.Goal{ID: id, Objective: "recover accepted candidate", AllowedRoot: root}
}
