package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

func TestWorktreeCreateNoticeExplainsSkippedLocalSetup(t *testing.T) {
	model := Model{}
	applyWorktreeMessages(&model, "worktree_create", []conversation.ServerMsg{{
		Type:     "worktree",
		Worktree: &workspace.Snapshot{ID: "workspace-id", Label: "feature", State: workspace.StateKept},
	}})

	for _, notice := range []string{model.Status, model.Events[0].Data.(sessionlog.Message).Text} {
		for _, expected := range []string{"settings", "hooks", ".worktreeinclude", "隔离工作树内手动配置"} {
			if !strings.Contains(notice, expected) {
				t.Fatalf("workspace creation notice %q does not explain %q", notice, expected)
			}
		}
	}
}
