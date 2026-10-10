package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceLifecycleExitTransitionRemainsIdempotentAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "baseline.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(projectRoot, "exit restart recovery")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	formal, err := filepath.Abs(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "lead-exit-recovery"
	openService := func() *Service {
		t.Helper()
		return newWorkspaceToolTransitionService(ctx, projectRoot, stateRoot, db, nil)
	}
	closeManagers := func(service *Service) {
		t.Helper()
		for _, manager := range service.workspaces {
			if err := manager.Close(ctx); err != nil {
				t.Errorf("close workspace manager: %v", err)
			}
		}
	}

	service := openService()
	_, scope, err := service.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{RunID: runID, SessionID: session.ID,
		AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(t.TempDir(), "candidate"), Mode: permission.ModeDefault}
	scope.OriginRunID = runID
	manager, err := service.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx, scope, "exit recovery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(stateRoot, scope.ProjectID, created.ID)
	paths := workspace.Paths{Root: workspaceRoot, Checkout: filepath.Join(workspaceRoot, "checkout")}
	const evidence = "retained through repeated exit recovery"
	evidencePath := filepath.Join(paths.Checkout, "evidence.txt")
	if err := os.WriteFile(evidencePath, []byte(evidence), 0600); err != nil {
		t.Fatal(err)
	}
	rootBefore, err := os.Stat(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	closeManagers(service)

	transition := appendPendingWorkspaceTransition(t, projectRoot, session.ID, runID, "exit-call", "exit workspace")
	if _, err := sessionlog.Append(projectRoot, session.ID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: transition.CallID, Result: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkspaceToolTerminal(t, projectRoot, session.ID, runID, 1); err != nil {
		t.Fatal(err)
	}

	firstRecovery := openService()
	if err := firstRecovery.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	firstManager, err := firstRecovery.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := firstManager.Binding(scope); err != nil || bound != "" {
		t.Fatalf("first recovery left workspace bound: bound=%q err=%v", bound, err)
	}
	firstRecord, err := readWorkspaceRecord(ctx, stateRoot, formal, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstRecord.Snapshot.State != workspace.StateReady {
		t.Fatalf("exit recovery changed lifecycle state: %+v", firstRecord.Snapshot)
	}
	firstCursor, firstUsed := firstRecord.Snapshot.Cursor, firstRecord.UsedBytes
	firstOperationID := firstRecord.Operation.ID
	if firstRecord.Operation.Kind != "create" || firstRecord.Operation.Phase != "complete" {
		t.Fatalf("exit recovery changed workspace operation: %+v", firstRecord.Operation)
	}
	firstTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestWorkspaceTransitions(firstTranscript.Events)[transition.ID]; got.Status != sessionlog.WorkspaceToolTransitionApplied {
		t.Fatalf("exit transition was not durably applied: %+v", got)
	}
	closeManagers(firstRecovery)

	// A second process restart must skip the already-applied transition and
	// leave workspace ownership, quota accounting, and checkout evidence intact.
	secondRecovery := openService()
	if err := secondRecovery.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	secondManager, err := secondRecovery.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := secondManager.Binding(scope); err != nil || bound != "" {
		t.Fatalf("repeated recovery changed empty binding: bound=%q err=%v", bound, err)
	}
	secondRecord, err := readWorkspaceRecord(ctx, stateRoot, formal, scope, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondRecord.Snapshot.State != workspace.StateReady || secondRecord.Snapshot.Cursor != firstCursor ||
		secondRecord.UsedBytes != firstUsed || secondRecord.Operation.ID != firstOperationID {
		t.Fatalf("repeated exit recovery changed workspace record: first=%+v second=%+v", firstRecord, secondRecord)
	}
	rootAfter, err := os.Stat(paths.Root)
	if err != nil || !os.SameFile(rootBefore, rootAfter) {
		t.Fatalf("repeated exit recovery changed workspace root identity: before=%v after=%v err=%v", rootBefore, rootAfter, err)
	}
	if got, err := os.ReadFile(evidencePath); err != nil || string(got) != evidence {
		t.Fatalf("repeated exit recovery lost checkout evidence: got=%q err=%v", got, err)
	}
	secondTranscript, err := sessionlog.Replay(projectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	transitionEvents := 0
	for _, event := range secondTranscript.Events {
		if event.Type != sessionlog.EventWorkspaceToolTransition {
			continue
		}
		var persisted sessionlog.WorkspaceToolTransition
		if decodeSessionData(event.Data, &persisted) == nil && persisted.ID == transition.ID {
			transitionEvents++
		}
	}
	if transitionEvents != 2 {
		t.Fatalf("repeated recovery appended transition receipt/status more than once: events=%d", transitionEvents)
	}
	closeManagers(secondRecovery)
}

func readWorkspaceRecord(ctx context.Context, stateRoot, formalRoot string, scope workspace.Scope, id string) (workspace.Record, error) {
	layout, err := workspace.NewLayout(stateRoot, formalRoot, scope.ProjectID)
	if err != nil {
		return workspace.Record{}, err
	}
	defer layout.Close()
	ownership, err := workspace.NewOwnershipStore(layout)
	if err != nil {
		return workspace.Record{}, err
	}
	defer ownership.Close()
	return ownership.Load(ctx, scope, id)
}
