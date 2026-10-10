package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/workspace"
)

// Workspace status rendering is metadata-only: private summaries and raw
// diff-like text remain outside the transcript UI, while bounded conflict
// paths and digests remain available for review.
func TestWorktreeStatusRenderingOmitsPrivateSummaryAndDiffBody(t *testing.T) {
	const (
		roleBody   = "PRIVATE ROLE BODY: inspect the unreleased parser roadmap"
		apiKey     = "sk-worktree-ui-private-fixture-0123456789"
		thinking   = "PRIVATE THINKING: the hidden customer records imply"
		transcript = "RAW CHILD TRANSCRIPT: customer@example.invalid"
		diffBody   = "@@ -1 +1 @@\n-private source line\n+private candidate line"
	)
	text := formatWorktreeMessages([]conversation.ServerMsg{{
		Type: "worktree",
		Worktree: &workspace.Snapshot{
			ID: "1123456789abcdef0123456789abcdef", Label: "review", State: workspace.StateKept,
			Generation: 4, ChangedFiles: 1, ConflictCount: 2, Conflicts: []string{"src/parser.go"},
			BaselineDigest: "baseline-digest", FormalDigest: "formal-digest", WorkspaceDigest: "workspace-digest",
			Summary: strings.Join([]string{roleBody, apiKey, thinking, transcript, diffBody}, "\n"),
		},
	}})

	for _, private := range []string{roleBody, apiKey, thinking, transcript, diffBody, "private source line", "private candidate line"} {
		if strings.Contains(text, private) {
			t.Errorf("workspace status exposed private content %q: %s", private, text)
		}
	}
	for _, visible := range []string{"review", "src/parser.go", "baseline-digest", "formal-digest", "workspace-digest", "冲突 2 项"} {
		if !strings.Contains(text, visible) {
			t.Errorf("workspace status omitted review metadata %q: %s", visible, text)
		}
	}
}
