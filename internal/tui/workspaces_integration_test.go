package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

type tuiWorkspacePassingChecker struct{}

func (tuiWorkspacePassingChecker) Check(context.Context, candidate.Candidate) (candidate.Finding, error) {
	return candidate.Finding{ID: "workspace-check", Checker: "workspace-test", Result: candidate.FindingPass}, nil
}

type tuiHeldRun struct {
	events   chan agent.ExecutionEvent
	done     chan agent.RunOutcome
	finished chan struct{}
	once     sync.Once
}

type tuiHoldingRunner struct {
	mu   sync.Mutex
	runs map[string]*tuiHeldRun
}

func (r *tuiHoldingRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	r.mu.Lock()
	if r.runs == nil {
		r.runs = map[string]*tuiHeldRun{}
	}
	run := &tuiHeldRun{events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1), finished: make(chan struct{})}
	r.runs[request.RunID] = run
	r.mu.Unlock()
	return &agent.RunHandle{Events: run.events, Done: run.done}, nil
}

func (r *tuiHoldingRunner) Cancel(runID string) error {
	r.mu.Lock()
	run := r.runs[runID]
	r.mu.Unlock()
	if run == nil {
		return nil
	}
	run.once.Do(func() {
		close(run.events)
		run.done <- agent.RunOutcome{RunID: runID, Status: agent.RunCancelled}
		close(run.done)
		close(run.finished)
	})
	return nil
}

func TestWorktreeTUICreateSurvivesConversationServiceRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "kept.txt"), []byte("formal baseline"), 0600); err != nil {
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
	workspaceState := filepath.Join(root, "workspace-state")
	socketDir, err := os.MkdirTemp("", "m09-tui-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &tuiHoldingRunner{}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceClosed := false
	cleanupService := func() {
		if !serviceClosed {
			if err := service.Close(); err != nil {
				t.Errorf("close conversation service: %v", err)
			}
			serviceClosed = true
		}
		stopService()
	}
	t.Cleanup(cleanupService)

	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "hold the lead run while creating a workspace", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = runner.Cancel(runID)
		_ = stream.Close()
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}
	runEnded := false
	t.Cleanup(func() {
		if !runEnded {
			_ = stream.Cancel(sessionID, runID)
			for {
				message, receiveErr := stream.Receive()
				if receiveErr != nil || message.Type == "run_outcome" {
					break
				}
			}
		}
		_ = stream.Close()
	})

	m := New(socket, formal)
	m.ActiveSession, m.ActiveRunID = sessionID, runID
	m.Pending = true
	m.Composer.SetValue("/worktrees create restart-check")
	updated, command := m.submitComposer()
	got := updated.(Model)
	if command == nil || got.ActiveRunID != runID || !got.Pending {
		t.Fatalf("TUI create did not preserve active lead run: run=%q pending=%v", got.ActiveRunID, got.Pending)
	}
	result, ok := command().(resultMsg)
	if !ok || result.err != nil {
		t.Fatalf("TUI worktree create result=%+v err=%v", result, result.err)
	}
	updated, _ = got.handleResult(result)
	got = updated.(Model)
	if len(got.Worktrees) != 1 || got.Worktrees[0].Label != "restart-check" {
		t.Fatalf("TUI did not create the service-owned workspace: %+v", got.Worktrees)
	}
	workspaceID := got.Worktrees[0].ID

	if err := stream.Cancel(sessionID, runID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive lead run cancellation: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("lead run outcome=%+v, want cancelled", message.Outcome)
			}
			runEnded = true
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	serviceClosed = true
	stopService()

	serviceCtx, stopService = context.WithCancel(ctx)
	service, err = conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
		SocketPath: socket, PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceClosed = false

	restored := New(socket, formal)
	restored.ActiveSession = sessionID
	restored.Composer.SetValue("/worktrees")
	updated, command = restored.submitComposer()
	restored = updated.(Model)
	if command == nil {
		t.Fatal("TUI did not issue the post-restart workspace list")
	}
	result = command().(resultMsg)
	if result.err != nil {
		t.Fatalf("list restored workspace: %v", result.err)
	}
	updated, _ = restored.handleResult(result)
	restored = updated.(Model)
	if len(restored.Worktrees) != 1 || restored.Worktrees[0].ID != workspaceID || restored.Worktrees[0].Label != "restart-check" {
		t.Fatalf("TUI did not restore the same workspace after restart: %+v", restored.Worktrees)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "kept.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("formal project changed during workspace lifecycle: content=%q err=%v", got, err)
	}
}

func TestWorktreeTUIResolutionExportAndAcceptanceUsesConversationService(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove socket directory: %v", err)
		}
	})
	socket := filepath.Join(socketDir, "c.sock")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
		CandidateCheckers: []candidate.Checker{tuiWorkspacePassingChecker{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	sessions, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: formal})
	if err != nil || len(sessions) != 1 || sessions[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	formalRoot, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalRoot)))
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: "p" + hex.EncodeToString(projectHash[:16]), SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Authority: permission.Authority{
			RunID: runID, SessionID: sessionID, AllowedRoot: formalRoot,
			FormalRoot: formalRoot, CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalRoot, scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	managerClosed := false
	t.Cleanup(func() {
		if !managerClosed {
			if err := manager.Close(ctx); err != nil {
				t.Errorf("close workspace fixture: %v", err)
			}
		}
	})
	created, err := manager.Create(ctx, scope, "TUI acceptance")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Close(ctx)
	managerClosed = true
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte("workspace version"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal version"), 0600); err != nil {
		t.Fatal(err)
	}

	m := New(socket, formal)
	m.ActiveSession = sessionID
	m.Composer.SetValue("/worktrees resolve " + created.ID)
	updated, cmd := m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("TUI did not request a worktree conflict preview")
	}
	updated, _ = m.handleResult(cmd().(resultMsg))
	m = updated.(Model)
	if m.WorktreeDialog == nil || m.WorktreeDialog.Mode != "resolve" || m.WorktreeDialog.Snapshot.Conflicts[0] != "board.txt" {
		t.Fatalf("TUI did not receive the service conflict preview: %+v", m.WorktreeDialog)
	}
	updated, _ = m.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m = updated.(Model)
	updated, cmd = m.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("TUI did not submit the explicit workspace path choice")
	}
	updated, _ = m.handleResult(cmd().(resultMsg))
	m = updated.(Model)

	m.Composer.SetValue("/worktrees export " + created.ID)
	updated, cmd = m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("TUI did not request candidate export")
	}
	updated, _ = m.handleResult(cmd().(resultMsg))
	m = updated.(Model)
	if len(m.Worktrees) != 1 || m.Worktrees[0].CandidateID == "" {
		t.Fatalf("TUI did not expose the exported candidate: %+v", m.Worktrees)
	}
	candidateID := m.Worktrees[0].CandidateID
	assertFileContent(t, filepath.Join(formal, "board.txt"), "formal version")
	candidateRecord, err := db.GetCandidate(ctx, candidateID)
	if err != nil || candidateRecord.Candidate.Status == "accepted" {
		t.Fatalf("candidate was accepted during export: record=%+v err=%v", candidateRecord, err)
	}

	m.Composer.SetValue("/review " + candidateID)
	updated, cmd = m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("TUI did not request candidate review")
	}
	updated, followup := m.handleResult(cmd().(resultMsg))
	m = updated.(Model)
	if followup == nil || m.Review == nil || m.Review.CandidateID != candidateID || len(m.Review.Findings) != 1 || m.Review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("TUI review response=%+v follow-up=%v", m.Review, followup != nil)
	}
	updated, _ = m.handleResult(followup().(resultMsg))
	m = updated.(Model)
	updated, cmd = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("TUI did not submit explicit candidate acceptance")
	}
	acceptResult := cmd().(resultMsg)
	if acceptResult.err != nil || len(acceptResult.msgs) != 1 || acceptResult.msgs[0].Type != "acceptance" {
		t.Fatalf("service acceptance result=%+v err=%v", acceptResult, acceptResult.err)
	}
	updated, _ = m.handleResult(acceptResult)
	m = updated.(Model)
	if m.Review != nil || m.Status != "候选已接收，目标仍需独立复核。" {
		t.Fatalf("TUI did not close the accepted review: review=%+v status=%q", m.Review, m.Status)
	}
	assertFileContent(t, filepath.Join(formal, "board.txt"), "workspace version")
	candidateRecord, err = db.GetCandidate(ctx, candidateID)
	if err != nil || candidateRecord.Candidate.Status != "accepted" {
		t.Fatalf("candidate status=%q err=%v; want accepted", candidateRecord.Candidate.Status, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file %s=%q err=%v; want %q", path, got, err, want)
	}
}
