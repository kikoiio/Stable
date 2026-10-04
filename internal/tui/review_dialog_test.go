package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/candidate"
)

func TestReviewDialogRequiresIndividualForceConfirmation(t *testing.T) {
	review := &candidate.Review{ID: "r", CandidateID: "c", Digest: "preview", CandidateDigest: "new", FormalDigest: "old", Changes: []candidate.FileChange{{Path: "board", Status: "modified", TextDiff: "--- old\n+++ new\n"}}, Findings: []candidate.Finding{{ID: "erc", Checker: "erc", Result: candidate.FindingFail, Reason: "short"}, {ID: "lint", Checker: "lint", Result: candidate.FindingUnavailable, Reason: "offline"}}}
	m := New("sock", "/project")
	m.Review, m.ReviewConfirmed = review, map[string]bool{}
	if got := renderReview(*review, m.ReviewConfirmed, 0, nil, false, 0, false, false, "", "", 120); !strings.Contains(got, "board") || !strings.Contains(got, "unavailable") {
		t.Fatalf("review details missing: %s", got)
	}
	updated, cmd := m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("force acceptance proceeded without confirmation")
	}
	if !strings.Contains(m.Status, "逐项确认") {
		t.Fatalf("status=%q", m.Status)
	}
	updated, _ = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	m = updated.(Model)
	updated, _ = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(Model)
	updated, _ = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	m = updated.(Model)
	if len(m.ReviewConfirmed) != 2 {
		t.Fatalf("confirmed=%v", m.ReviewConfirmed)
	}
	_, cmd = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if cmd == nil {
		t.Fatal("complete force confirmation did not submit")
	}
}
