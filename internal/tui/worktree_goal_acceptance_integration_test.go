package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/store"
)

func TestTUIAcceptingGoalCandidateQueuesIndependentReverification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.txt"), []byte("approved baseline"), 0600); err != nil {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-goal-accept-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	serviceCtx, stopService := context.WithCancel(ctx)
	service, err := conversation.Serve(serviceCtx, conversation.Deps{
		Store: db, ProjectRoot: formal, SocketPath: socket, PollEvery: time.Hour,
		CandidateCheckers: []candidate.Checker{tuiWorkspacePassingChecker{}},
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
		t.Fatalf("create source session: messages=%+v err=%v", sessions, err)
	}
	sessionID := sessions[0].Session.ID
	goalID := "goal-tui-candidate-recheck"
	goal, err := db.CreateGoal(ctx, core.Goal{
		ID: goalID, Objective: "verify the accepted design independently", AllowedRoot: formal,
		SourceSessionID: sessionID, Status: core.GoalActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(root, "candidates")
	candidateItem, err := candidate.CreateCandidateForPolicy("goal-tui-candidate", formal, candidateRoot, candidate.ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateItem.CandidateRoot, "board.txt"), []byte("candidate design"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateItem, err = candidate.FreezeCandidate(candidateItem, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidateItem.Status = "ready"
	if err := db.SaveCandidate(ctx, store.CandidateRecord{
		Candidate: candidateItem, ActionID: "goal-tui-candidate-action", GoalID: goalID,
	}); err != nil {
		t.Fatal(err)
	}

	model := New(socket, formal)
	model.ActiveSession = sessionID
	model.Composer.SetValue("/review " + candidateItem.ID)
	updated, command := model.submitComposer()
	model = updated.(Model)
	if command == nil {
		t.Fatal("TUI did not start Goal candidate review")
	}
	updated, followup := model.handleResult(command().(resultMsg))
	model = updated.(Model)
	if followup == nil || model.Review == nil || model.Review.CandidateID != candidateItem.ID {
		t.Fatalf("Goal candidate review response=%+v followup=%v", model.Review, followup != nil)
	}
	updated, _ = model.handleResult(followup().(resultMsg))
	model = updated.(Model)
	beforeAccept, err := db.GetGoalSnapshot(ctx, goalID)
	if err != nil || beforeAccept.Goal.Status != goal.Goal.Status || beforeAccept.Goal.Revision != goal.Goal.Revision {
		t.Fatalf("review changed Goal before explicit acceptance: before=%+v after=%+v err=%v", goal.Goal, beforeAccept.Goal, err)
	}
	if model.Review == nil || len(model.Review.Findings) != 1 || model.Review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("candidate review is not ready for ordinary acceptance: %+v", model.Review)
	}
	updated, command = model.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	if command == nil {
		t.Fatal("TUI did not submit explicit acceptance")
	}
	acceptResult := command().(resultMsg)
	if acceptResult.err != nil || len(acceptResult.msgs) != 1 || acceptResult.msgs[0].Type != "acceptance" {
		t.Fatalf("Goal candidate acceptance response=%+v err=%v", acceptResult.msgs, acceptResult.err)
	}
	updated, _ = model.handleResult(acceptResult)
	model = updated.(Model)
	acceptedGoal, err := db.GetGoalSnapshot(ctx, goalID)
	if err != nil || acceptedGoal.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("acceptance did not queue independent Goal recheck: goal=%+v err=%v", acceptedGoal.Goal, err)
	}
	if model.Review != nil || model.Status != "候选已接收，目标仍需独立复核。" {
		t.Fatalf("TUI overstated Goal completion after candidate acceptance: review=%+v status=%q", model.Review, model.Status)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "board.txt")); err != nil || string(got) != "candidate design" {
		t.Fatalf("accepted candidate content=%q err=%v", got, err)
	}
}
