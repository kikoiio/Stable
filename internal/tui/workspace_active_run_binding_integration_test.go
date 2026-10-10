package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type tuiTrustedHoldingRunner struct {
	*tuiHoldingRunner
}

func (r *tuiTrustedHoldingRunner) StartWithExecutorFactory(ctx context.Context, request agent.ExecutionRequest, _ agent.ExecutorFactory) (*agent.RunHandle, error) {
	return r.Start(ctx, request)
}

type tuiBindingAllowGate struct{}

func (tuiBindingAllowGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

func TestWorktreeTUIEnterIsBlockedAndExitWaitsDuringActiveRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-binding-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &tuiTrustedHoldingRunner{tuiHoldingRunner: &tuiHoldingRunner{}}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
			Sandbox: sandbox.New(), Gate: tuiBindingAllowGate{},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
		stopService()
	})

	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID

	openRun := func() (*conversation.StreamClient, string) {
		t.Helper()
		runID, err := sessionlog.NewID()
		if err != nil {
			t.Fatal(err)
		}
		stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
			RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
			Intent: "hold the session while checking workspace binding", Model: "fixture",
		})
		if err != nil {
			t.Fatalf("open run: %v", err)
		}
		started, err := stream.Receive()
		if err != nil || started.Type != "run_started" || started.RunID != runID {
			t.Fatalf("start run: message=%+v err=%v", started, err)
		}
		return stream, runID
	}
	finishRun := func(stream *conversation.StreamClient, runID string) {
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
			t.Fatalf("close run %s stream: %v", runID, err)
		}
	}
	applyCommand := func(m Model, line string, wantError bool) Model {
		t.Helper()
		parentRunID, pending, cursor, parentStream := m.ActiveRunID, m.Pending, m.LastCursor, m.stream
		m.Composer.SetValue(line)
		updated, command := m.submitComposer()
		m = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not dispatch %s", line)
		}
		rawResult := command()
		result, ok := rawResult.(resultMsg)
		if !ok {
			t.Fatalf("TUI command %s returned %T", line, rawResult)
		}
		if wantError && result.err == nil {
			t.Fatalf("%s unexpectedly succeeded: %+v", line, result.msgs)
		}
		if !wantError && result.err != nil {
			t.Fatalf("%s failed: %v", line, result.err)
		}
		updated, _ = m.handleResult(result)
		m = updated.(Model)
		if parentRunID != "" && (m.ActiveRunID != parentRunID || m.Pending != pending || m.LastCursor != cursor || m.stream != parentStream) {
			t.Fatalf("%s changed parent run state: run=%q pending=%v cursor=%d stream-preserved=%v", line, m.ActiveRunID, m.Pending, m.LastCursor, m.stream == parentStream)
		}
		if wantError && !strings.Contains(m.Status, "工作树请求失败") {
			t.Fatalf("%s failure was not visible in TUI: status=%q", line, m.Status)
		}
		return m
	}

	// Create two independent workspaces during a normal session run, then bind
	// one while idle so the held run below starts with a real writer lease.
	createStream, createRunID := openRun()
	model := New(socket, formal)
	model.ActiveSession, model.ActiveRunID, model.Pending, model.stream = sessionID, createRunID, true, createStream
	model = applyCommand(model, "/worktrees create bound-one", false)
	if len(model.Worktrees) != 1 {
		t.Fatalf("first workspace missing after create: %+v", model.Worktrees)
	}
	firstID := model.Worktrees[0].ID
	model = applyCommand(model, "/worktrees create bound-two", false)
	if len(model.Worktrees) != 2 {
		t.Fatalf("second workspace missing after create: %+v", model.Worktrees)
	}
	secondID := ""
	for _, item := range model.Worktrees {
		if item.ID != firstID {
			secondID = item.ID
		}
	}
	if secondID == "" {
		t.Fatalf("second workspace ID missing: %+v", model.Worktrees)
	}
	finishRun(createStream, createRunID)

	idle := New(socket, formal)
	idle.ActiveSession = sessionID
	idle = applyCommand(idle, "/worktrees enter "+firstID, false)

	activeStream, activeRunID := openRun()
	active := New(socket, formal)
	active.ActiveSession, active.ActiveRunID, active.Pending, active.stream = sessionID, activeRunID, true, activeStream
	active.LastCursor = 19
	active = applyCommand(active, "/worktrees get "+firstID, false)
	if len(active.Worktrees) != 1 || active.Worktrees[0].State != workspace.StateWriting || active.Worktrees[0].WriterRunID != activeRunID || active.Worktrees[0].Generation == 0 {
		t.Fatalf("held run did not use the first workspace writer lease: %+v", active.Worktrees)
	}
	active = applyCommand(active, "/worktrees enter "+secondID, true)
	if active.Err == nil || active.Err.Error() != workspace.ErrUnavailable.Error() {
		t.Fatalf("active-run binding guard returned %v, want %q", active.Err, workspace.ErrUnavailable)
	}
	active = applyCommand(active, "/worktrees get "+firstID, false)
	if len(active.Worktrees) != 1 || active.Worktrees[0].State != workspace.StateWriting || active.Worktrees[0].WriterRunID != activeRunID {
		t.Fatalf("rejected enter changed the active binding or writer: %+v", active.Worktrees)
	}
	active = applyCommand(active, "/worktrees exit", false)
	for {
		message, err := activeStream.Receive()
		if err != nil {
			t.Fatalf("receive exit-triggered run terminal: %v", err)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("exit-triggered run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := activeStream.Close(); err != nil {
		t.Fatalf("close active run stream: %v", err)
	}

	// Exit must settle the active writer before releasing the binding, allowing
	// a later TUI request to bind the second workspace.
	idle = New(socket, formal)
	idle.ActiveSession = sessionID
	idle = applyCommand(idle, "/worktrees enter "+secondID, false)
}
