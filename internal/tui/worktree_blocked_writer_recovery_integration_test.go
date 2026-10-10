package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type blockedWriterHeldRun struct {
	events chan agent.ExecutionEvent
	done   chan agent.RunOutcome
	once   sync.Once
}

type blockedWriterTUIRunner struct {
	mu             sync.Mutex
	runs           map[string]*blockedWriterHeldRun
	failCancelFor  string
	allowRetryStop bool
}

func (r *blockedWriterTUIRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	run := &blockedWriterHeldRun{events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1)}
	r.mu.Lock()
	if r.runs == nil {
		r.runs = map[string]*blockedWriterHeldRun{}
	}
	r.runs[request.RunID] = run
	r.mu.Unlock()
	return &agent.RunHandle{Events: run.events, Done: run.done}, nil
}

func (r *blockedWriterTUIRunner) StartWithExecutorFactory(ctx context.Context, request agent.ExecutionRequest, _ agent.ExecutorFactory) (*agent.RunHandle, error) {
	return r.Start(ctx, request)
}

func (r *blockedWriterTUIRunner) Cancel(runID string) error {
	r.mu.Lock()
	if runID == r.failCancelFor && !r.allowRetryStop {
		r.mu.Unlock()
		return errors.New("sandbox process did not exit")
	}
	run := r.runs[runID]
	r.mu.Unlock()
	if run != nil {
		run.once.Do(func() {
			close(run.events)
			run.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
			close(run.done)
		})
	}
	return nil
}

func TestWorktreeTUIShowsBlockedWriterReasonAndRetainsCheckoutUntilRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal baseline"), 0600); err != nil {
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
	socketParent, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(socketParent, 0700); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(socketParent, "tui-blocked-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	workspaceState := filepath.Join(root, "workspace-state")
	runner := &blockedWriterTUIRunner{}
	serviceCtx, cancelService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		ExecutorFactory: execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Gate: tuiBindingAllowGate{}, Sandbox: sandbox.New()}),
	})
	if err != nil {
		cancelService()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runner.mu.Lock()
		runner.allowRetryStop = true
		activeRuns := make([]string, 0, len(runner.runs))
		for runID := range runner.runs {
			activeRuns = append(activeRuns, runID)
		}
		runner.mu.Unlock()
		for _, runID := range activeRuns {
			_ = runner.Cancel(runID)
		}
	})
	serviceClosed := false
	t.Cleanup(func() {
		if !serviceClosed {
			if err := service.Close(); err != nil {
				t.Errorf("close conversation service: %v", err)
			}
		}
		cancelService()
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
			Intent: "hold for blocked-worktree recovery", Model: "fixture",
			Messages: []llm.Message{{Role: "user", Content: "hold"}},
		})
		if err != nil {
			t.Fatalf("open run: %v", err)
		}
		for {
			started, err := stream.Receive()
			if err != nil {
				t.Fatalf("start run %s: %v", runID, err)
			}
			if started.Type == "error" || started.Type == "done" {
				t.Fatalf("run %s failed before start: %+v", runID, started)
			}
			if started.Type == "run_started" {
				if started.RunID != runID {
					t.Fatalf("started run ID=%q, want %q", started.RunID, runID)
				}
				return stream, runID
			}
		}
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
					t.Fatalf("run %s outcome=%+v, want canceled", runID, message.Outcome)
				}
				break
			}
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close run %s stream: %v", runID, err)
		}
	}
	applyWorktree := func(model Model, line string, wantError bool) Model {
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
		updated, _ = updated.(Model).handleResult(result)
		return updated.(Model)
	}

	// A live session run is required to create a workspace from the TUI.
	setupStream, setupRunID := openRun()
	setupModel := New(socket, formal)
	setupModel.ActiveSession, setupModel.ActiveRunID, setupModel.Pending, setupModel.stream = sessionID, setupRunID, true, setupStream
	setupModel = applyWorktree(setupModel, "/worktrees create blocked-recovery", false)
	if len(setupModel.Worktrees) != 1 {
		t.Fatalf("created workspace missing from TUI: %+v", setupModel.Worktrees)
	}
	workspaceID := setupModel.Worktrees[0].ID
	finishRun(setupStream, setupRunID)

	idleModel := New(socket, formal)
	idleModel.ActiveSession = sessionID
	idleModel = applyWorktree(idleModel, "/worktrees enter "+workspaceID, false)
	boundStream, boundRunID := openRun()
	runner.mu.Lock()
	runner.failCancelFor = boundRunID
	runner.mu.Unlock()

	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectDigest := sha256.Sum256([]byte(filepath.Clean(formalAbs)))
	projectID := "p" + hex.EncodeToString(projectDigest[:16])
	sentinel := filepath.Join(workspaceState, projectID, workspaceID, "checkout", "user-data.txt")
	if err := os.WriteFile(sentinel, []byte("preserve while writer is unconfirmed"), 0600); err != nil {
		t.Fatal(err)
	}
	activeModel := New(socket, formal)
	activeModel.ActiveSession, activeModel.ActiveRunID, activeModel.Pending, activeModel.stream = sessionID, boundRunID, true, boundStream
	activeModel = applyWorktree(activeModel, "/worktrees exit", true)
	if !strings.Contains(activeModel.Status, "工作树请求失败") {
		t.Fatalf("failed writer stop was not visible: status=%q", activeModel.Status)
	}

	getModel := New(socket, formal)
	getModel.ActiveSession = sessionID
	getModel = applyWorktree(getModel, "/worktrees get "+workspaceID, false)
	if len(getModel.Worktrees) != 1 {
		t.Fatalf("blocked workspace missing from TUI: %+v", getModel.Worktrees)
	}
	blocked := getModel.Worktrees[0]
	if blocked.State != workspace.StateBlocked || blocked.WriterRunID != boundRunID || blocked.Generation == 0 || !strings.Contains(blocked.Error, "sandbox process did not exit") {
		t.Fatalf("failed stop did not retain a visible blocked lease: %+v", blocked)
	}
	visibleReason := false
	for _, event := range getModel.Events {
		message, ok := event.Data.(sessionlog.Message)
		if ok && strings.Contains(message.Text, "状态说明：sandbox process did not exit") {
			visibleReason = true
		}
	}
	if !visibleReason {
		t.Fatalf("TUI transcript omitted blocked stop reason: %+v", getModel.Events)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve while writer is unconfirmed" {
		t.Fatalf("failed stop changed checkout user data: content=%q err=%v", got, err)
	}

	runner.mu.Lock()
	runner.allowRetryStop = true
	runner.mu.Unlock()
	retryModel := New(socket, formal)
	retryModel.ActiveSession = sessionID
	retryModel = applyWorktree(retryModel, "/worktrees exit", false)
	for {
		message, err := boundStream.Receive()
		if err != nil {
			t.Fatalf("receive retried run terminal: %v", err)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("retried run outcome=%+v, want canceled", message.Outcome)
			}
			break
		}
	}
	if err := boundStream.Close(); err != nil {
		t.Fatal(err)
	}
	finalModel := New(socket, formal)
	finalModel.ActiveSession = sessionID
	finalModel = applyWorktree(finalModel, "/worktrees get "+workspaceID, false)
	if len(finalModel.Worktrees) != 1 || finalModel.Worktrees[0].State != workspace.StateKept || finalModel.Worktrees[0].WriterRunID != "" {
		t.Fatalf("retry did not settle and retain the workspace: %+v", finalModel.Worktrees)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve while writer is unconfirmed" {
		t.Fatalf("successful retry lost retained checkout data: content=%q err=%v", got, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	serviceClosed = true
	cancelService()
}
