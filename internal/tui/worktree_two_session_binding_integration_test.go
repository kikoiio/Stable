package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUITwoSessionsKeepBindingsAndQueriesIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	cwdBefore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	socketDir, err := os.MkdirTemp("/tmp", "m09-two-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove socket directory: %v", err)
		}
	})
	socket := filepath.Join(socketDir, "chat.sock")
	runner := &tuiTrustedHoldingRunner{tuiHoldingRunner: &tuiHoldingRunner{}}
	service, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Gate: tuiBindingAllowGate{}, Sandbox: sandbox.New(),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	newSession := func() sessionlog.SessionInfo {
		t.Helper()
		messages, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
		if err != nil || len(messages) != 1 || messages[0].Session == nil {
			t.Fatalf("create session: messages=%+v err=%v", messages, err)
		}
		return *messages[0].Session
	}
	sessionA, sessionB := newSession(), newSession()
	if sessionA.ID == sessionB.ID {
		t.Fatalf("session IDs collided: %q", sessionA.ID)
	}

	model := New(socket, formal)
	model.Sessions = []sessionlog.SessionInfo{sessionA, sessionB}
	createForSession := func(session sessionlog.SessionInfo) string {
		t.Helper()
		if model.ActiveSession == "" {
			model.ActiveSession = session.ID
		} else if model.ActiveSession != session.ID {
			targetIndex := 0
			for index, candidate := range model.Sessions {
				if candidate.ID == session.ID {
					targetIndex = index
					break
				}
			}
			delta := targetIndex - model.sessionIndex()
			model.SelectedSession(delta)
			if model.ActiveSession != session.ID {
				t.Fatalf("TUI did not select session %s: active=%q", session.ID, model.ActiveSession)
			}
			model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
		}
		stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
			Work:   agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
			Intent: "create a session-owned workspace", Model: "fixture",
		})
		if err != nil {
			t.Fatalf("open lead run for %s: %v", session.ID, err)
		}
		started, err := stream.Receive()
		if err != nil || started.Type != "run_started" {
			_ = stream.Close()
			t.Fatalf("start lead run for %s: message=%+v err=%v", session.ID, started, err)
		}
		model.ActiveSession, model.ActiveRunID, model.Pending, model.stream = session.ID, started.RunID, true, stream
		model, _ = runWorktreeTUICommand(t, model, "/worktrees create same-label", false)
		if len(model.Worktrees) != 1 || model.Worktrees[0].Label != "same-label" {
			t.Fatalf("session %s did not create its labeled workspace: %+v", session.ID, model.Worktrees)
		}
		workspaceID := model.Worktrees[0].ID
		if !workspace.ValidID(workspaceID) {
			t.Fatalf("service returned invalid workspace ID %q", workspaceID)
		}
		if err := stream.Cancel(session.ID, started.RunID); err != nil {
			t.Fatalf("cancel creation run for %s: %v", session.ID, err)
		}
		for {
			message, receiveErr := stream.Receive()
			if receiveErr != nil {
				t.Fatalf("receive creation run terminal for %s: %v", session.ID, receiveErr)
			}
			if message.Type == "run_outcome" {
				if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
					t.Fatalf("creation run for %s outcome=%+v, want cancelled", session.ID, message.Outcome)
				}
				break
			}
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close creation stream for %s: %v", session.ID, err)
		}
		model.ActiveRunID, model.Pending, model.stream = "", false, nil
		return workspaceID
	}

	workspaceA := createForSession(sessionA)
	workspaceB := createForSession(sessionB)
	if workspaceA == workspaceB {
		t.Fatalf("same label produced the same service ID: %q", workspaceA)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, workspaceProjectID(formalAbs))
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	pathsA, err := layout.Paths(workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	pathsB, err := layout.Paths(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if pathsA.Root == pathsB.Root || filepath.Base(pathsA.Root) != workspaceA || filepath.Base(pathsB.Root) != workspaceB {
		t.Fatalf("workspace paths are not keyed by distinct service IDs: A=%q B=%q", pathsA.Root, pathsB.Root)
	}
	if filepath.Base(pathsA.Root) == "same-label" || filepath.Base(pathsB.Root) == "same-label" {
		t.Fatalf("user label was used as a workspace path: A=%q B=%q", pathsA.Root, pathsB.Root)
	}

	model.SelectedSession(-1)
	if model.ActiveSession != sessionA.ID {
		t.Fatalf("TUI did not switch to session A: active=%q", model.ActiveSession)
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
	model, _ = runWorktreeTUICommand(t, model, "/worktrees enter "+workspaceA, false)
	model.SelectedSession(1)
	if model.ActiveSession != sessionB.ID {
		t.Fatalf("TUI did not switch to session B: active=%q", model.ActiveSession)
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
	model, _ = runWorktreeTUICommand(t, model, "/worktrees enter "+workspaceB, false)
	model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceB {
		t.Fatalf("session switch did not replace the TUI list with session B's workspaces: %+v", model.Worktrees)
	}
	model, _ = runWorktreeTUICommand(t, model, "/worktrees get "+workspaceB, false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceB {
		t.Fatalf("session B could not get its own workspace: %+v", model.Worktrees)
	}
	if _, result := runWorktreeTUICommand(t, model, "/worktrees get "+workspaceA, true); result.err == nil {
		t.Fatal("session B TUI read session A's workspace")
	}
	if _, result := runWorktreeTUICommand(t, model, "/worktrees enter "+workspaceA, true); result.err == nil {
		t.Fatal("session B TUI entered session A's workspace")
	}
	model.SelectedSession(-1)
	model, _ = runWorktreeTUICommand(t, model, "/worktrees", false)
	if len(model.Worktrees) != 1 || model.Worktrees[0].ID != workspaceA {
		t.Fatalf("switching back to session A did not restore its isolated list: %+v", model.Worktrees)
	}
	if _, result := runWorktreeTUICommand(t, model, "/worktrees get "+workspaceA, false); result.err != nil {
		t.Fatalf("session A could not get its own workspace after the session switch: %v", result.err)
	}

	// Start one bound lead run in each session at the same time. RunStarted
	// persists the exact workspace ID and generation selected by each binding.
	streamA, runA := openTUIWorkspaceRun(t, ctx, socket, sessionA.ID)
	streamB, runB := openTUIWorkspaceRun(t, ctx, socket, sessionB.ID)
	for _, owner := range []struct{ session, run, workspaceID string }{
		{sessionA.ID, runA, workspaceA}, {sessionB.ID, runB, workspaceB},
	} {
		transcript, err := sessionlog.Replay(formal, owner.session)
		if err != nil {
			t.Fatal(err)
		}
		var matched *sessionlog.RunStarted
		for _, event := range transcript.Events {
			if event.Type != sessionlog.EventRunStarted {
				continue
			}
			var started sessionlog.RunStarted
			if decodeTUIWorkspaceEvent(event.Data, &started) == nil && started.RunID == owner.run {
				matched = &started
				break
			}
		}
		if matched == nil || matched.WorkspaceID != owner.workspaceID || matched.WorkspaceGeneration == 0 {
			gotID, generation := "", uint64(0)
			if matched != nil {
				gotID, generation = matched.WorkspaceID, matched.WorkspaceGeneration
			}
			t.Fatalf("session %s run used wrong binding: run=%q workspace=%q want=%q generation=%d", owner.session, owner.run, gotID, owner.workspaceID, generation)
		}
	}
	finishTUIWorkspaceRun(t, streamA, sessionA.ID, runA)
	finishTUIWorkspaceRun(t, streamB, sessionB.ID, runB)

	for _, session := range []sessionlog.SessionInfo{sessionA, sessionB} {
		listed, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "worktree_list", SessionID: session.ID})
		if err != nil || len(listed) != 1 || len(listed[0].Worktrees) != 1 {
			t.Fatalf("list session %s after bound runs: messages=%+v err=%v", session.ID, listed, err)
		}
		wantID := workspaceA
		if session.ID == sessionB.ID {
			wantID = workspaceB
		}
		if listed[0].Worktrees[0].ID != wantID || listed[0].Worktrees[0].State != workspace.StateKept {
			t.Fatalf("session %s workspace state/binding crossed: got=%+v want ID=%q kept", session.ID, listed[0].Worktrees[0], wantID)
		}
	}
	cwdAfter, err := os.Getwd()
	if err != nil || cwdAfter != cwdBefore {
		t.Fatalf("worktree flow changed process cwd: before=%q after=%q err=%v", cwdBefore, cwdAfter, err)
	}
}

func runWorktreeTUICommand(t *testing.T, model Model, line string, wantError bool) (Model, resultMsg) {
	t.Helper()
	model.Composer.SetValue(line)
	updated, command := model.submitComposer()
	if command == nil {
		t.Fatalf("TUI did not dispatch %s", line)
	}
	result, ok := command().(resultMsg)
	if !ok {
		t.Fatalf("TUI command %s returned an unexpected result", line)
	}
	if wantError && result.err == nil {
		t.Fatalf("%s unexpectedly succeeded: %+v", line, result.msgs)
	}
	if !wantError && result.err != nil {
		t.Fatalf("%s failed: %v", line, result.err)
	}
	model, modelOK := updated.(Model)
	if !modelOK {
		t.Fatalf("TUI command %s returned model %T", line, updated)
	}
	updated, _ = model.handleResult(result)
	return updated.(Model), result
}

func openTUIWorkspaceRun(t *testing.T, ctx context.Context, socket, sessionID string) (*conversation.StreamClient, string) {
	t.Helper()
	stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		Work:   agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "verify independent session workspace binding", Model: "fixture",
	})
	if err != nil {
		t.Fatalf("open bound run for %s: %v", sessionID, err)
	}
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID == "" {
		_ = stream.Close()
		t.Fatalf("start bound run for %s: message=%+v err=%v", sessionID, started, err)
	}
	return stream, started.RunID
}

func finishTUIWorkspaceRun(t *testing.T, stream *conversation.StreamClient, sessionID, runID string) {
	t.Helper()
	if err := stream.Cancel(sessionID, runID); err != nil {
		t.Fatalf("cancel run %s: %v", runID, err)
	}
	for {
		message, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive run %s terminal: %v", runID, err)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("run %s outcome=%+v, want cancelled", runID, message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close run stream %s: %v", runID, err)
	}
}

func workspaceProjectID(formalRoot string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(formalRoot)))
	return "p" + hex.EncodeToString(digest[:16])
}

func decodeTUIWorkspaceEvent(data any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
