package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUIExportIsIdempotentAfterConversationRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "workspace-state")
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	socketDir, err := os.MkdirTemp("", "m09-tui-export-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &tuiHoldingRunner{}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: stateRoot,
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
		CandidateCheckers: []candidate.Checker{tuiWorkspacePassingChecker{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceClosed := false
	t.Cleanup(func() {
		if !serviceClosed {
			if err := service.Close(); err != nil {
				t.Errorf("close conversation service: %v", err)
			}
		}
		stopService()
	})

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
	t.Cleanup(func() { _ = stream.Close() })
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, formal)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, runID, true
	apply := func(line string) Model {
		t.Helper()
		model.Composer.SetValue(line)
		updated, command := model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not dispatch %s", line)
		}
		result, ok := command().(resultMsg)
		if !ok || result.err != nil {
			t.Fatalf("TUI command %s returned result=%+v", line, result)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		return model
	}
	model = apply("/worktrees create idempotent-export")
	if len(model.Worktrees) != 1 {
		t.Fatalf("workspace create response=%+v", model.Worktrees)
	}
	workspaceID := model.Worktrees[0].ID
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
			break
		}
	}
	model.ActiveRunID, model.Pending, model.stream = "", false, nil

	formalRoot, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalRoot)))
	projectID := "p" + hex.EncodeToString(projectHash[:16])
	layout, err := workspace.NewLayout(stateRoot, formalRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := layout.Paths(workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := workspace.NewOwnershipStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: projectID, SessionID: sessionID,
		Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "board.txt"), []byte("workspace change"), 0600); err != nil {
		t.Fatal(err)
	}
	model = apply("/worktrees export " + workspaceID)
	if len(model.Worktrees) != 1 || model.Worktrees[0].CandidateID == "" || model.Worktrees[0].State != workspace.StateExported {
		t.Fatalf("first export response=%+v", model.Worktrees)
	}
	candidateID := model.Worktrees[0].CandidateID
	candidateRecord, err := db.GetCandidate(ctx, candidateID)
	if err != nil || candidateRecord.Candidate.Status != "ready" {
		t.Fatalf("export candidate=%+v err=%v", candidateRecord, err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "board.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("export changed formal bytes before review/accept: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(candidateRecord.Candidate.CandidateRoot, "board.txt")); err != nil || string(got) != "workspace change" {
		t.Fatalf("export candidate bytes=%q err=%v", got, err)
	}
	firstJournal, err := ownership.Load(ctx, scope, workspaceID)
	if err != nil || firstJournal.Operation.Kind != "export" || firstJournal.Operation.Phase != "complete" || firstJournal.Operation.ID == "" {
		t.Fatalf("completed export journal=%+v err=%v", firstJournal.Operation, err)
	}

	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	serviceClosed = true
	stopService()
	serviceCtx, stopService = context.WithCancel(ctx)
	service, err = conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: stateRoot,
		SocketPath: socket, PollEvery: time.Hour,
		CandidateCheckers: []candidate.Checker{tuiWorkspacePassingChecker{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceClosed = false

	model = New(socket, formal)
	model.ActiveSession = sessionID
	model = apply("/worktrees get " + workspaceID)
	if len(model.Worktrees) != 1 || model.Worktrees[0].State != workspace.StateExported || model.Worktrees[0].CandidateID != candidateID {
		t.Fatalf("workspace export was not recovered after restart: %+v", model.Worktrees)
	}
	restartedJournal, err := ownership.Load(ctx, scope, workspaceID)
	if err != nil || restartedJournal.Operation.ID != firstJournal.Operation.ID || restartedJournal.Operation.Phase != "complete" {
		t.Fatalf("restart changed completed export operation: first=%+v restarted=%+v err=%v", firstJournal.Operation, restartedJournal.Operation, err)
	}
	model = apply("/worktrees export " + workspaceID)
	if len(model.Worktrees) != 1 || model.Worktrees[0].State != workspace.StateExported || model.Worktrees[0].CandidateID != candidateID {
		t.Fatalf("repeated export did not return the durable candidate: %+v", model.Worktrees)
	}
	candidateAfterRestart, err := db.GetCandidate(ctx, candidateID)
	if err != nil || candidateAfterRestart.Candidate.CandidateDigest != candidateRecord.Candidate.CandidateDigest || candidateAfterRestart.Candidate.Status != "ready" {
		t.Fatalf("repeated export changed candidate identity/content state: before=%+v after=%+v err=%v", candidateRecord.Candidate, candidateAfterRestart.Candidate, err)
	}
	repeatedJournal, err := ownership.Load(ctx, scope, workspaceID)
	if err != nil || repeatedJournal.Operation.ID != firstJournal.Operation.ID || repeatedJournal.Operation.Phase != "complete" {
		t.Fatalf("idempotent export changed the durable operation: first=%+v repeated=%+v err=%v", firstJournal.Operation, repeatedJournal.Operation, err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "board.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("repeated export changed formal bytes: %q err=%v", got, err)
	}
}
