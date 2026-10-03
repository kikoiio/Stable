package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
	"stable/internal/store"
)

func TestStorePermissionGatePersistsRulesAcrossCandidateRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project := filepath.Join(root, "project")
	c1 := filepath.Join(root, "candidate-1")
	c2 := filepath.Join(root, "candidate-2")
	for _, p := range []string{project, c1, c2} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ids := 0
	gate := StorePermissionGate{Store: state, NewID: func() string { ids++; return fmt.Sprintf("approval-%d", ids) }}
	a1 := permission.Authority{RunID: "run-1", SessionID: "session", GoalID: "goal", WorkItemID: "action-1", AllowedRoot: project, FormalRoot: project, CandidateRoot: c1, Mode: permission.ModeDefault}
	op1 := permission.Operation{ID: "edit-1", Kind: permission.OpLegacy, Name: "kicad.repair", Target: filepath.Join(c1, "board.kicad_sch"), Parameters: json.RawMessage(`{"fix":"wire"}`)}
	d, err := gate.Authorize(ctx, a1, op1)
	if err != nil || d.Kind != permission.DecisionAsk {
		t.Fatalf("initial decision=%+v err=%v", d, err)
	}
	service := permission.PermissionService{Repository: state}
	if _, err = service.ResolveApproval(ctx, d.ApprovalID, permission.ChoiceSaveRule, permission.UserPrincipal{SessionID: "session", UserID: "local", Authenticated: true}, a1, op1); err != nil {
		t.Fatal(err)
	}
	a2 := a1
	a2.RunID = "run-2"
	a2.WorkItemID = "action-2"
	a2.CandidateRoot = c2
	op2 := op1
	op2.ID = "edit-2"
	op2.Target = filepath.Join(c2, "board.kicad_sch")
	d, err = gate.Authorize(ctx, a2, op2)
	if err != nil || d.Kind != permission.DecisionAllow {
		t.Fatalf("saved exact rule was not reused: %+v err=%v", d, err)
	}
	op3 := op2
	op3.ID = "edit-3"
	op3.Parameters = json.RawMessage(`{"fix":"other"}`)
	d, err = gate.Authorize(ctx, a2, op3)
	if err != nil || d.Kind != permission.DecisionAsk {
		t.Fatalf("changed parameters reused saved rule: %+v err=%v", d, err)
	}
}

func TestStorePermissionGateConsumesOneTimeGrantOnActionRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	project, candidate := filepath.Join(root, "project"), filepath.Join(root, "candidate")
	for _, p := range []string{project, candidate} {
		if err = os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a := permission.Authority{RunID: "stable-action", SessionID: "session", GoalID: "goal", WorkItemID: "action", AllowedRoot: project, FormalRoot: project, CandidateRoot: candidate, Mode: permission.ModeDefault}
	o := permission.Operation{ID: "action-write", Kind: permission.OpLegacy, Name: "repair", Target: filepath.Join(candidate, "board")}
	gate := StorePermissionGate{Store: state, NewID: func() string { return "approval-once" }}
	d, err := gate.Authorize(ctx, a, o)
	if err != nil || d.Kind != permission.DecisionAsk {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	if err = state.ResolveApproval(ctx, d.ApprovalID, permission.ApprovalAllowedOnce, d.ScopeDigest, d.OperationDigest, nil); err != nil {
		t.Fatal(err)
	}
	d, err = gate.Authorize(ctx, a, o)
	if err != nil || d.Kind != permission.DecisionAllow {
		t.Fatalf("retry did not consume approval: %+v err=%v", d, err)
	}
	if err = state.ConsumeApproval(ctx, "approval-once", d.ScopeDigest, d.OperationDigest); err == nil {
		t.Fatal("one-time approval was consumed more than once")
	}
}
