package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorktreeTUISequentialAcceptsPreserveIndependentWorkspaceChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"first.txt": "first baseline", "second.txt": "second baseline"} {
		if err := os.WriteFile(filepath.Join(formal, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
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
	socketDir, err := os.MkdirTemp("", "m09-tui-sequential-accept-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove socket directory: %v", err)
		}
	})
	socket := filepath.Join(socketDir, "conversation.sock")
	runner := &tuiHoldingRunner{}
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
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
		Intent: "create two independent workspace candidates", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamClosed := false
	runEnded := false
	t.Cleanup(func() {
		if !runEnded {
			_ = runner.Cancel(runID)
			_ = stream.Cancel(sessionID, runID)
			for {
				message, receiveErr := stream.Receive()
				if receiveErr != nil || message.Type == "run_outcome" {
					break
				}
			}
		}
		if !streamClosed {
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, formal)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, runID, true
	createWorkspace := func(label string) workspace.Snapshot {
		t.Helper()
		model.Composer.SetValue("/worktrees create " + label)
		updated, command := model.submitComposer()
		model = updated.(Model)
		if command == nil || model.ActiveRunID != runID || !model.Pending {
			t.Fatalf("create %q did not retain the authenticated lead run: active=%q pending=%v", label, model.ActiveRunID, model.Pending)
		}
		result, ok := command().(resultMsg)
		if !ok || result.err != nil {
			t.Fatalf("create %q: result=%+v err=%v", label, result, result.err)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		if len(model.Worktrees) == 0 || model.Worktrees[len(model.Worktrees)-1].Label != label {
			t.Fatalf("TUI did not expose created workspace %q: %+v", label, model.Worktrees)
		}
		return model.Worktrees[len(model.Worktrees)-1]
	}
	first := createWorkspace("first-change")
	second := createWorkspace("second-change")
	if first.ID == second.ID {
		t.Fatalf("TUI returned the same workspace ID twice: %q", first.ID)
	}

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
	streamClosed = true

	formalRoot, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalRoot)))
	projectID := "p" + hex.EncodeToString(projectHash[:16])
	layout, err := workspace.NewLayout(workspaceState, formalRoot, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	for _, change := range []struct{ id, name, content string }{
		{id: first.ID, name: "first.txt", content: "first accepted value"},
		{id: second.ID, name: "second.txt", content: "second pending value"},
	} {
		paths, pathErr := layout.Paths(change.id)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if err := os.WriteFile(filepath.Join(paths.Checkout, change.name), []byte(change.content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	model = New(socket, formal)
	model.ActiveSession = sessionID
	acceptCandidate := func(workspaceID string, wantCandidateFiles map[string]string) string {
		t.Helper()
		model.Composer.SetValue("/worktrees preview " + workspaceID)
		updated, command := model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not request preview for workspace %s", workspaceID)
		}
		result := command().(resultMsg)
		if result.err != nil {
			t.Fatalf("preview workspace %s: %v", workspaceID, result.err)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		if model.WorktreeDialog != nil {
			t.Fatalf("non-conflicting workspace %s unexpectedly opened resolution dialog: %+v", workspaceID, model.WorktreeDialog)
		}
		var snapshot *workspace.Snapshot
		for _, msg := range result.msgs {
			if msg.Type == "worktree" && msg.Worktree != nil {
				snapshot = msg.Worktree
				break
			}
		}
		if snapshot == nil || snapshot.ConflictCount != 0 {
			t.Fatalf("workspace %s preview=%+v; want a conflict-free preview", workspaceID, snapshot)
		}

		model.Composer.SetValue("/worktrees export " + workspaceID)
		updated, command = model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not request export for workspace %s", workspaceID)
		}
		result = command().(resultMsg)
		if result.err != nil {
			t.Fatalf("export workspace %s: %v", workspaceID, result.err)
		}
		updated, _ = model.handleResult(result)
		model = updated.(Model)
		candidateID := ""
		for _, item := range model.Worktrees {
			if item.ID == workspaceID {
				candidateID = item.CandidateID
			}
		}
		if candidateID == "" {
			t.Fatalf("TUI did not expose candidate for workspace %s: %+v", workspaceID, model.Worktrees)
		}
		for name, want := range wantCandidateFiles {
			candidateRecord, getErr := db.GetCandidate(ctx, candidateID)
			if getErr != nil {
				t.Fatalf("get candidate %s: %v", candidateID, getErr)
			}
			assertFileContent(t, filepath.Join(candidateRecord.Candidate.CandidateRoot, name), want)
		}

		model.Composer.SetValue("/review " + candidateID)
		updated, command = model.submitComposer()
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not request review for candidate %s", candidateID)
		}
		updated, followup := model.handleResult(command().(resultMsg))
		model = updated.(Model)
		if followup == nil || model.Review == nil || model.Review.CandidateID != candidateID || len(model.Review.Findings) != 1 || model.Review.Findings[0].Result != candidate.FindingPass {
			t.Fatalf("candidate %s review=%+v follow-up=%v", candidateID, model.Review, followup != nil)
		}
		updated, _ = model.handleResult(followup().(resultMsg))
		model = updated.(Model)
		updated, command = model.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		model = updated.(Model)
		if command == nil {
			t.Fatalf("TUI did not request explicit acceptance for candidate %s", candidateID)
		}
		acceptResult := command().(resultMsg)
		if acceptResult.err != nil || len(acceptResult.msgs) != 1 || acceptResult.msgs[0].Type != "acceptance" {
			t.Fatalf("accept candidate %s: result=%+v err=%v", candidateID, acceptResult, acceptResult.err)
		}
		updated, _ = model.handleResult(acceptResult)
		model = updated.(Model)
		if model.Review != nil {
			t.Fatalf("TUI retained accepted review for %s: %+v", candidateID, model.Review)
		}
		return candidateID
	}

	firstCandidate := acceptCandidate(first.ID, map[string]string{
		"first.txt": "first accepted value", "second.txt": "second baseline",
	})
	firstRecord, err := db.GetCandidate(ctx, firstCandidate)
	if err != nil || firstRecord.Candidate.Status != "accepted" {
		t.Fatalf("first candidate status=%q err=%v; want accepted", firstRecord.Candidate.Status, err)
	}
	assertFileContent(t, filepath.Join(formal, "first.txt"), "first accepted value")
	assertFileContent(t, filepath.Join(formal, "second.txt"), "second baseline")

	secondCandidate := acceptCandidate(second.ID, map[string]string{
		"first.txt": "first accepted value", "second.txt": "second pending value",
	})
	if secondCandidate == firstCandidate {
		t.Fatalf("second workspace reused candidate %q", firstCandidate)
	}
	secondRecord, err := db.GetCandidate(ctx, secondCandidate)
	if err != nil || secondRecord.Candidate.Status != "accepted" {
		t.Fatalf("second candidate status=%q err=%v; want accepted", secondRecord.Candidate.Status, err)
	}
	assertFileContent(t, filepath.Join(formal, "first.txt"), "first accepted value")
	assertFileContent(t, filepath.Join(formal, "second.txt"), "second pending value")
}
