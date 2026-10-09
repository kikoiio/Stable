package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type workspaceLifecycleAllowGate struct{}

func (workspaceLifecycleAllowGate) Authorize(_ context.Context, _ permission.Authority, op permission.Operation) (permission.PermissionDecision, error) {
	if op.Kind != permission.OpWorkspaceLifecycle {
		return permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "unexpected operation kind"}, nil
	}
	return permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "test approval"}, nil
}

type workspaceLifecycleDecisionGate struct {
	decision permission.PermissionDecision
}

type workspaceLifecycleCancelRunner struct {
	done     chan struct{}
	canceled chan string
	once     sync.Once
}

func (*workspaceLifecycleCancelRunner) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, nil
}

func (r *workspaceLifecycleCancelRunner) Cancel(runID string) error {
	r.canceled <- runID
	r.once.Do(func() { close(r.done) })
	return nil
}

func (g workspaceLifecycleDecisionGate) Authorize(_ context.Context, _ permission.Authority, _ permission.Operation) (permission.PermissionDecision, error) {
	return g.decision, nil
}

func TestWorkspaceLifecyclePermissionFailureHasNoDurableSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision permission.PermissionDecision
	}{
		{name: "denied", decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "denied by test"}},
		{name: "unapproved ask", decision: permission.PermissionDecision{Kind: permission.DecisionAsk}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "baseline.txt"), []byte("formal"), 0600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			session, err := sessionlog.Create(root, "lifecycle permission")
			if err != nil {
				t.Fatal(err)
			}
			request := agent.ExecutionRequest{RunID: "lead-denied-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "denied workspace create"}
			authority, err := BuildAuthority(ctx, db, root, request, permission.ModeDefault, "")
			if err != nil {
				t.Fatal(err)
			}
			request.PermissionBounds, err = json.Marshal(authority)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: request.RunID, WorkKind: "session", Intent: request.Intent}); err != nil {
				t.Fatal(err)
			}
			stateRoot := filepath.Join(t.TempDir(), "workspace-state")
			host := execution.NewWorkspaceLifecycleToolHost()
			svc := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, host)
			svc.activeRuns[request.RunID] = session.ID
			svc.activeRequests[request.RunID] = request
			_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := svc.workspaceService(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := manager.Close(context.Background()); err != nil {
					t.Errorf("close workspace manager: %v", err)
				}
			})
			factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: workspaceLifecycleDecisionGate{decision: tc.decision}, SessionRoot: root}, execution.WithWorkspaceLifecycleToolHost(host))
			runner, err := factory.ForRun(request)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.Execute(ctx, llm.ToolUse{ID: "denied-enter-call", Name: "enter_worktree", Arguments: json.RawMessage(`{"label":"must not exist"}`)})
			if err != nil || outcome.Status == agent.ToolSucceeded || !outcome.IsError {
				t.Fatalf("unauthorized lifecycle outcome=%+v err=%v", outcome, err)
			}
			transcript, err := sessionlog.Replay(root, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range transcript.Events {
				if event.Type == sessionlog.EventWorkspaceToolTransition {
					t.Fatalf("unauthorized operation persisted a transition: %+v", event)
				}
			}
			created, err := manager.List(ctx, scope, 0, 10)
			if err != nil || len(created) != 0 {
				t.Fatalf("unauthorized operation created a workspace: %+v err=%v", created, err)
			}
		})
	}
}

func TestWorkspaceTransitionRecoveryPairsSequentialReusedCallIDs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "reused tool call ID recovery")
	if err != nil {
		t.Fatal(err)
	}
	callID := "provider-call-reused"
	first := appendPendingWorkspaceTransition(t, root, session.ID, "lead-first", callID, "first exit")
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: callID, Result: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, first.RunID, 1); err != nil {
		t.Fatal(err)
	}
	first.Status, first.UpdatedAt = sessionlog.WorkspaceToolTransitionApplied, time.Now().UTC()
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventWorkspaceToolTransition, first); err != nil {
		t.Fatal(err)
	}
	second := appendPendingWorkspaceTransition(t, root, session.ID, "lead-second", callID, "second exit")
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: callID, Result: "scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, second.RunID, 1); err != nil {
		t.Fatal(err)
	}
	service := newWorkspaceToolTransitionService(ctx, root, filepath.Join(t.TempDir(), "workspace-state"), db, nil)
	if err := service.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest := latestWorkspaceTransitions(transcript.Events)
	if got := latest[first.ID]; got.Status != sessionlog.WorkspaceToolTransitionApplied {
		t.Fatalf("first reused-ID transition changed: %+v", got)
	}
	if got := latest[second.ID]; got.Status != sessionlog.WorkspaceToolTransitionApplied {
		t.Fatalf("second reused-ID transition was not applied: %+v", got)
	}
	for _, manager := range service.workspaces {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close recovered workspace manager: %v", err)
		}
	}
}

func appendPendingWorkspaceTransition(t *testing.T, root, sessionID, runID, callID, intent string) sessionlog.WorkspaceToolTransition {
	t.Helper()
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: "session", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventToolCall, sessionlog.ToolCall{RunID: runID, CallID: callID, Name: "exit_worktree"}); err != nil {
		t.Fatal(err)
	}
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	transition := sessionlog.WorkspaceToolTransition{ID: id, SessionID: sessionID, RunID: runID, CallID: callID, WorkKind: "session", Action: "exit", Status: sessionlog.WorkspaceToolTransitionPending, CreatedAt: now, UpdatedAt: now}
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventWorkspaceToolTransition, transition); err != nil {
		t.Fatal(err)
	}
	return transition
}

func TestWorkspaceLifecycleEnterDefersBindingAndReplaysExactlyOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "baseline.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "deferred workspace tool")
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: "lead-enter-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "enter workspace"}
	authority, err := BuildAuthority(ctx, db, root, request, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds, err = json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: request.RunID, WorkKind: "session", Intent: request.Intent}); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	host := execution.NewWorkspaceLifecycleToolHost()
	svc := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, host)
	svc.activeRuns[request.RunID] = session.ID
	svc.activeRequests[request.RunID] = request
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: workspaceLifecycleAllowGate{}, SessionRoot: root}, execution.WithWorkspaceLifecycleToolHost(host))
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(ctx, llm.ToolUse{ID: "enter-call", Name: "enter_worktree", Arguments: json.RawMessage(`{"label":"created by lead"}`)})
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
		t.Fatalf("enter tool outcome=%+v err=%v", outcome, err)
	}
	if outcome.Content == "" || !containsText(outcome.Content, "scheduled") {
		t.Fatalf("enter tool did not report deferred result: %q", outcome.Content)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := manager.Binding(scope)
	if err != nil || bound != "" {
		t.Fatalf("tool call changed binding before lead terminal: bound=%q err=%v", bound, err)
	}
	created, err := manager.List(ctx, scope, 0, 10)
	if err != nil || len(created) != 1 || created[0].State != workspace.StateReady {
		t.Fatalf("workspace create was not durable before enter: snapshots=%+v err=%v", created, err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, request.RunID, 1); err != nil {
		t.Fatal(err)
	}

	// Simulate a process stop after the terminal fact but before applying the
	// binding transition. Startup recovery must enter the already-created ID.
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, nil)
	if err := restarted.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	newManager, err := restarted.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := newManager.Close(context.Background()); err != nil {
			t.Errorf("close restarted workspace manager: %v", err)
		}
	})
	newBound, err := newManager.Binding(scope)
	if err != nil || newBound != created[0].ID {
		t.Fatalf("restart did not apply exact pending binding: bound=%q want=%q err=%v", newBound, created[0].ID, err)
	}
	if got, err := newManager.List(ctx, scope, 0, 10); err != nil || len(got) != 1 || got[0].ID != created[0].ID {
		t.Fatalf("restart duplicated workspace create: snapshots=%+v err=%v", got, err)
	}
	if err := restarted.reconcileWorkspaceToolTransitions(ctx, session.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := newManager.List(ctx, scope, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("repeated reconciliation was not idempotent: snapshots=%+v err=%v", got, err)
	}
}

func TestWorkspaceLifecycleExitDefersBindingUntilLeadTerminal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "baseline.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "deferred workspace exit")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	host := execution.NewWorkspaceLifecycleToolHost()
	svc := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, host)
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	managerClosed := false
	t.Cleanup(func() {
		if !managerClosed {
			if err := manager.Close(context.Background()); err != nil {
				t.Errorf("close workspace manager: %v", err)
			}
		}
	})
	request := agent.ExecutionRequest{RunID: "lead-exit-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "exit workspace"}
	authority, err := BuildAuthority(ctx, db, root, request, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds, err = json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority, scope.OriginRunID = authority, request.RunID
	created, err := manager.Create(ctx, scope, "bound before exit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: request.RunID, WorkKind: "session", Intent: request.Intent}); err != nil {
		t.Fatal(err)
	}
	svc.activeRuns[request.RunID] = session.ID
	svc.activeRequests[request.RunID] = request
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: workspaceLifecycleAllowGate{}, SessionRoot: root}, execution.WithWorkspaceLifecycleToolHost(host))
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(ctx, llm.ToolUse{ID: "exit-call", Name: "exit_worktree", Arguments: json.RawMessage(`{}`)})
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
		t.Fatalf("exit tool outcome=%+v err=%v", outcome, err)
	}
	if bound, err := manager.Binding(scope); err != nil || bound != created.ID {
		t.Fatalf("exit changed binding before terminal: bound=%q want=%q err=%v", bound, created.ID, err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, request.RunID, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	managerClosed = true
	restarted := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, nil)
	if err := restarted.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	recoveredManager, err := restarted.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recoveredManager.Close(context.Background()); err != nil {
			t.Errorf("close recovered workspace manager: %v", err)
		}
	})
	if bound, err := recoveredManager.Binding(scope); err != nil || bound != "" {
		t.Fatalf("terminal exit did not clear binding after restart: bound=%q err=%v", bound, err)
	}
}

func TestWorkspaceLifecycleExportDefersOwnWriterAndReplaysIdempotently(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "deferred export")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	host := execution.NewWorkspaceLifecycleToolHost()
	svc := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, host)
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	managerClosed := false
	t.Cleanup(func() {
		if !managerClosed {
			if err := manager.Close(context.Background()); err != nil {
				t.Errorf("close workspace manager: %v", err)
			}
		}
	})
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	formal, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	runID := "lead-export-run"
	authority := permission.Authority{RunID: runID, SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(t.TempDir(), "candidate"), Mode: permission.ModeDefault}
	scope.Authority, scope.OriginRunID = authority, runID
	created, err := manager.Create(ctx, scope, "export after run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(stateRoot, formal, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "file.txt"), []byte("workspace edit"), 0600); err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	_ = layout.Close()
	lease, err := manager.AcquireLeadWriter(ctx, scope, created.ID, runID)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: runID, Work: scope.Work, Intent: "export workspace", AllowedScope: []string{lease.Authority.AllowedRoot}}
	request.PermissionBounds, err = json.Marshal(lease.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: "session", Intent: request.Intent, WorkspaceID: created.ID, WorkspaceGeneration: lease.Generation}); err != nil {
		t.Fatal(err)
	}
	svc.activeRuns[runID] = session.ID
	svc.activeRequests[runID] = request
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: workspaceLifecycleAllowGate{}, SessionRoot: root, WorkspaceLease: &lease, WorkspaceAccounting: manager}, execution.WithWorkspaceLifecycleToolHost(host))
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(ctx, llm.ToolUse{ID: "export-call", Name: "worktree_export", Arguments: json.RawMessage(`{"workspace_id":"` + created.ID + `"}`)})
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded || !strings.Contains(outcome.Content, "scheduled") {
		t.Fatalf("export tool outcome=%+v err=%v", outcome, err)
	}
	stillWriting, err := manager.Get(ctx, lease.Scope, created.ID)
	if err != nil || stillWriting.State != workspace.StateWriting || stillWriting.WriterRunID != runID || stillWriting.CandidateID != "" {
		t.Fatalf("export stopped its own active lead writer or exported early: snapshot=%+v err=%v", stillWriting, err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, runID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReleaseCompletedWriter(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	managerClosed = true
	restarted := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, nil)
	if err := restarted.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	recoveredManager, err := restarted.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := recoveredManager.Close(context.Background()); err != nil {
			t.Errorf("close recovered workspace manager: %v", err)
		}
	})
	recovered, err := recoveredManager.Get(ctx, scope, created.ID)
	if err != nil || recovered.State != workspace.StateExported || recovered.CandidateID == "" {
		t.Fatalf("deferred export was not recovered: snapshot=%+v err=%v", recovered, err)
	}
	if _, err := db.GetCandidate(ctx, recovered.CandidateID); err != nil {
		t.Fatalf("exported candidate missing after recovery: %v", err)
	}
	if err := restarted.recoverWorkspaceToolTransitions(); err != nil {
		t.Fatal(err)
	}
	again, err := recoveredManager.Get(ctx, scope, created.ID)
	if err != nil || again.CandidateID != recovered.CandidateID {
		t.Fatalf("replay duplicated or changed exported candidate: first=%+v second=%+v err=%v", recovered, again, err)
	}
}

func TestWorkspaceLifecycleExportStopsOtherTrustedWriterAfterLeadTerminal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("formal"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "deferred export stops task writer")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	host := execution.NewWorkspaceLifecycleToolHost()
	svc := newWorkspaceToolTransitionService(ctx, root, stateRoot, db, host)
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close workspace manager: %v", err)
		}
	})
	lead := agent.ExecutionRequest{RunID: "lead-export-after-task", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "export after task writer"}
	authority, err := BuildAuthority(ctx, db, root, lead, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	lead.PermissionBounds, err = json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	_, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority, scope.OriginRunID = authority, lead.RunID
	created, err := manager.Create(ctx, scope, "writer stop before export")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, scope, created.ID); err != nil {
		t.Fatal(err)
	}
	writerRunID := "trusted-task-writer"
	writerRequest := lead
	writerRequest.RunID = writerRunID
	writerAuthority, err := BuildAuthority(ctx, db, root, writerRequest, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	writerScope := scope
	writerScope.Authority, writerScope.OriginRunID = writerAuthority, writerRunID
	lease, err := manager.AcquireLeadWriter(ctx, writerScope, created.ID, writerRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Paths.Checkout, "file.txt"), []byte("task edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: lead.RunID, WorkKind: "session", Intent: lead.Intent}); err != nil {
		t.Fatal(err)
	}
	svc.activeRuns[lead.RunID] = session.ID
	svc.activeRequests[lead.RunID] = lead
	writerDone := make(chan struct{})
	runner := &workspaceLifecycleCancelRunner{done: writerDone, canceled: make(chan string, 1)}
	svc.deps.Runner = runner
	svc.workspaceRuns[writerRunID] = workspaceLeadRun{lease: lease, manager: manager}
	svc.runDone[writerRunID] = writerDone
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: workspaceLifecycleAllowGate{}, SessionRoot: root}, execution.WithWorkspaceLifecycleToolHost(host))
	leadExecutor, err := factory.ForRun(lead)
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolUse{ID: "deferred-export", Name: "worktree_export", Arguments: json.RawMessage(`{"workspace_id":"` + created.ID + `"}`)}
	outcome, err := leadExecutor.Execute(ctx, call)
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded || !strings.Contains(outcome.Content, "scheduled") {
		t.Fatalf("export tool outcome=%+v err=%v", outcome, err)
	}
	if snapshot, err := manager.Get(ctx, scope, created.ID); err != nil || snapshot.WriterRunID != writerRunID || snapshot.CandidateID != "" {
		t.Fatalf("export stopped another writer before lead terminal: snapshot=%+v err=%v", snapshot, err)
	}
	if err := appendWorkspaceToolTerminal(t, root, session.ID, lead.RunID, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.reconcileWorkspaceToolTransitions(ctx, session.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case canceled := <-runner.canceled:
		if canceled != writerRunID {
			t.Fatalf("stopped run=%q, want %q", canceled, writerRunID)
		}
	default:
		t.Fatal("deferred export did not stop the active trusted writer")
	}
	snapshot, err := manager.Get(ctx, scope, created.ID)
	if err != nil || snapshot.State != workspace.StateExported || snapshot.CandidateID == "" || snapshot.WriterRunID != "" {
		t.Fatalf("deferred export did not wait for writer and export: snapshot=%+v err=%v", snapshot, err)
	}
	if _, err := db.GetCandidate(ctx, snapshot.CandidateID); err != nil {
		t.Fatalf("deferred export candidate missing: %v", err)
	}
}

func newWorkspaceToolTransitionService(ctx context.Context, root, stateRoot string, db *store.Store, host *execution.WorkspaceLifecycleToolHost) *Service {
	service := &Service{
		deps:    Deps{Store: db, ProjectRoot: root, WorkspaceStateRoot: stateRoot, WorkspaceLifecycleHost: host},
		lifeCtx: ctx, activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{},
		runDone: map[string]chan struct{}{}, clients: map[chan ServerMsg]*clientSubscription{},
		workspaces: map[string]*workspace.LifecycleService{}, workspaceRuns: map[string]workspaceLeadRun{},
	}
	if host != nil {
		host.Bind(service.executeWorkspaceLifecycleTool)
	}
	return service
}

func appendWorkspaceToolTerminal(t *testing.T, root, sessionID, runID string, seq uint64) error {
	t.Helper()
	returnAppendID, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: returnAppendID, RunID: runID, SessionID: sessionID, RunSeq: seq, At: time.Now().UTC(),
		Kind: "terminal", Payload: map[string]any{"status": "completed"},
	})
	return err
}

func containsText(value, substr string) bool {
	return strings.Contains(value, substr)
}
