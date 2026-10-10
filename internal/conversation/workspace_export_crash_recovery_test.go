package conversation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

const workspaceExportCrashChildEnv = "STABLE_M09_WORKSPACE_EXPORT_CRASH_CHILD"

// The child blocks after replacing exactly the first file in the candidate.
// The parent kills it so neither exporter cleanup nor service Close can run.
func TestWorkspaceExportCrashAfterFirstMergedFileRecoversConservatively(t *testing.T) {
	if os.Getenv(workspaceExportCrashChildEnv) == "1" {
		runWorkspaceExportCrashChild(t)
		return
	}

	root := t.TempDir()
	formal := filepath.Join(root, "project")
	stateRoot := filepath.Join(root, "workspace-state")
	for _, path := range []string{formal} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"a-change.txt": "formal a",
		"b-stable.txt": "formal b",
		"c-change.txt": "formal c",
	} {
		if err := os.WriteFile(filepath.Join(formal, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(formal, "workspace export crash recovery")
	if err != nil {
		t.Fatal(err)
	}
	const runID = "lead-export-crash"
	const callID = "workspace-export-crash-call"
	sessionID := session.ID
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: "lead-run", SessionID: sessionID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}

	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	conversationService := newWorkspaceToolTransitionService(context.Background(), formal, stateRoot, db, nil)
	_, scope, err = conversationService.workspaceScope(context.Background(), ClientMsg{SessionID: sessionID})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{RunID: runID, SessionID: sessionID, AllowedRoot: formalAbs, FormalRoot: formalAbs, CandidateRoot: filepath.Join(root, "candidate")}
	layout, err := workspace.NewLayout(stateRoot, formalAbs, scope.ProjectID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{Exporter: workspaceCandidateExporter{service: conversationService}})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), scope, "export crash recovery")
	if err != nil {
		_ = manager.Close(context.Background())
		_ = db.Close()
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"a-change.txt": "workspace a",
		"c-change.txt": "workspace c",
	} {
		if err := os.WriteFile(filepath.Join(paths.Checkout, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sessionlog.Append(formal, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: string(agent.WorkSession), Intent: "workspace export crash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(formal, sessionID, sessionlog.EventToolCall, sessionlog.ToolCall{RunID: runID, CallID: callID, Name: "worktree_export"}); err != nil {
		t.Fatal(err)
	}
	transitionID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	transition := sessionlog.WorkspaceToolTransition{ID: transitionID, SessionID: sessionID, RunID: runID, CallID: callID, WorkKind: string(agent.WorkSession), Action: "export", WorkspaceID: created.ID, Status: sessionlog.WorkspaceToolTransitionPending, CreatedAt: now, UpdatedAt: now}
	if _, err := sessionlog.Append(formal, sessionID, sessionlog.EventWorkspaceToolTransition, transition); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(formal, sessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: callID, Result: "Workspace lifecycle operation durably scheduled"}); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkspaceToolTerminal(t, formal, sessionID, runID, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	markerPath := filepath.Join(root, "first-merged-entry")
	child := exec.Command(os.Args[0], "-test.run=^TestWorkspaceExportCrashAfterFirstMergedFileRecoversConservatively$", "-test.v")
	child.Env = append(os.Environ(),
		workspaceExportCrashChildEnv+"=1",
		"STABLE_M09_WORKSPACE_EXPORT_ROOT="+root,
		"STABLE_M09_WORKSPACE_EXPORT_FORMAL="+formalAbs,
		"STABLE_M09_WORKSPACE_EXPORT_STATE="+stateRoot,
		"STABLE_M09_WORKSPACE_EXPORT_SESSION="+sessionID,
		"STABLE_M09_WORKSPACE_EXPORT_ID="+created.ID,
		"STABLE_M09_WORKSPACE_EXPORT_RUN="+runID,
		"STABLE_M09_WORKSPACE_EXPORT_MARKER="+markerPath,
	)
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	childWaited := false
	t.Cleanup(func() {
		if !childWaited && child.Process != nil {
			_ = child.Process.Kill()
			<-childDone
			childWaited = true
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			break
		}
		select {
		case childErr := <-childDone:
			childWaited = true
			t.Fatalf("export helper exited before first merged entry: %v", childErr)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("export helper did not reach the crash boundary: %v", err)
	}
	if strings.TrimSpace(string(marker)) != "a-change.txt" {
		t.Fatalf("crash hook observed entry %q, want first sorted merged entry", marker)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill exporter helper at durable crash boundary: %v", err)
	}
	childErr := <-childDone
	childWaited = true
	var exitErr *exec.ExitError
	if !errors.As(childErr, &exitErr) {
		t.Fatalf("export helper exit=%v, want hard process termination", childErr)
	}

	candidateParent := filepath.Join(root, ".stable-candidates")
	candidateDirs, err := os.ReadDir(candidateParent)
	if err != nil || len(candidateDirs) != 1 || !candidateDirs[0].IsDir() {
		t.Fatalf("partial candidate dirs=%v err=%v, want exactly one retained directory", candidateDirs, err)
	}
	partialRoot := filepath.Join(candidateParent, candidateDirs[0].Name())
	rootIdentity, err := candidate.CaptureRootIdentity(partialRoot)
	if err != nil {
		t.Fatalf("capture crashed partial root identity: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(partialRoot, "a-change.txt")); err != nil || string(data) != "workspace a" {
		t.Fatalf("first merged file=%q err=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(partialRoot, "c-change.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later merged file exists before its copy boundary: %v", err)
	}

	openRecovered := func() (*Service, *workspace.LifecycleService, *store.Store, workspace.Scope) {
		t.Helper()
		reopenedDB, openErr := store.Open(filepath.Join(root, "state.db"))
		if openErr != nil {
			t.Fatal(openErr)
		}
		recoveredService := newWorkspaceToolTransitionService(context.Background(), formal, stateRoot, reopenedDB, nil)
		if openErr := recoveredService.recoverWorkspaceToolTransitions(); openErr != nil {
			_ = reopenedDB.Close()
			t.Fatal(openErr)
		}
		_, recoveredScope, openErr := recoveredService.workspaceScope(context.Background(), ClientMsg{SessionID: sessionID})
		if openErr != nil {
			_ = reopenedDB.Close()
			t.Fatal(openErr)
		}
		recoveredScope.Authority = scope.Authority
		recoveredManager, openErr := recoveredService.workspaceService(formalAbs)
		if openErr != nil {
			_ = reopenedDB.Close()
			t.Fatal(openErr)
		}
		return recoveredService, recoveredManager, reopenedDB, recoveredScope
	}

	var priorCursor uint64
	for recovery := 0; recovery < 2; recovery++ {
		recoveredService, recovered, reopenedDB, recoveredScope := openRecovered()
		snapshot, getErr := recovered.Get(context.Background(), recoveredScope, created.ID)
		if getErr != nil || snapshot.State != workspace.StateInterrupted || snapshot.CandidateID != "" || snapshot.Error != "service restarted during export; resource retained for explicit recovery" {
			t.Fatalf("recovery %d workspace snapshot=%+v err=%v; want interrupted without a published candidate", recovery+1, snapshot, getErr)
		}
		transcript, replayErr := sessionlog.Replay(formal, sessionID)
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		finalTransition := latestWorkspaceTransitions(transcript.Events)[transitionID]
		if finalTransition.Status != sessionlog.WorkspaceToolTransitionFailed || finalTransition.CandidateID != "" {
			t.Fatalf("recovery %d transition falsely completed: %+v", recovery+1, finalTransition)
		}
		if recovery == 0 {
			priorCursor = snapshot.Cursor
		} else if snapshot.Cursor != priorCursor {
			t.Fatalf("second recovery advanced terminal state: cursor %d -> %d", priorCursor, snapshot.Cursor)
		}
		if _, err := reopenedDB.GetCandidate(context.Background(), candidateDirs[0].Name()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("recovery %d registered partial candidate: %v", recovery+1, err)
		}
		for path, workspaceManager := range recoveredService.workspaces {
			if err := workspaceManager.Close(context.Background()); err != nil {
				t.Errorf("close recovered conversation workspace service: %v", err)
			}
			delete(recoveredService.workspaces, path)
		}
		if err := reopenedDB.Close(); err != nil {
			t.Fatal(err)
		}
		currentIdentity, identityErr := candidate.CaptureRootIdentity(partialRoot)
		if identityErr != nil || currentIdentity != rootIdentity {
			t.Fatalf("recovery %d changed/removed unowned partial candidate identity: %q err=%v", recovery+1, currentIdentity, identityErr)
		}
	}
}

func runWorkspaceExportCrashChild(t *testing.T) {
	root := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_ROOT")
	formal := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_FORMAL")
	stateRoot := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_STATE")
	sessionID := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_SESSION")
	workspaceID := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_ID")
	runID := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_RUN")
	markerPath := os.Getenv("STABLE_M09_WORKSPACE_EXPORT_MARKER")
	if root == "" || formal == "" || stateRoot == "" || sessionID == "" || workspaceID == "" || runID == "" || markerPath == "" {
		t.Fatal("missing workspace export crash-helper parameters")
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := newWorkspaceToolTransitionService(context.Background(), formal, stateRoot, db, nil)
	_, scope, err := service.workspaceScope(context.Background(), ClientMsg{SessionID: sessionID})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	scope.Authority = permission.Authority{
		RunID: runID, SessionID: sessionID,
		AllowedRoot: formalAbs, FormalRoot: formalAbs,
		CandidateRoot: filepath.Join(root, "candidate"),
	}
	exporter := workspaceCandidateExporter{service: service, afterCandidateEntry: func(name string) {
		if err := os.WriteFile(markerPath, []byte(name), 0600); err != nil {
			t.Fatalf("write export crash marker: %v", err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}}
	layout, err := workspace.NewLayout(stateRoot, formalAbs, scope.ProjectID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{Exporter: exporter})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_, _ = manager.Export(context.Background(), scope, workspaceID)
	t.Fatal("export returned instead of stopping at the injected crash boundary")
}
