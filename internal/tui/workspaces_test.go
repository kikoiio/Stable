package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
	"stable/internal/workspace"
)

func TestWorktreePreviewCommandUsesUserSessionScope(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	m := New("", t.TempDir())
	m.ActiveSession = sessionID
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "worktree"})
	m.Socket = socket
	m.Composer.SetValue("/worktrees preview " + workspaceID)

	updated, command := m.submitComposer()
	got := updated.(Model)
	if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatalf("preview command changed parent run state: pending=%v run=%q cursor=%d", got.Pending, got.ActiveRunID, got.LastCursor)
	}
	result := command().(resultMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	request := agentFixtureRequest(t, requests)
	if request.Op != "worktree_preview" || request.SessionID != sessionID || request.WorkKind != "session" || request.ID != workspaceID || request.RunID != "" || request.Run != nil {
		t.Fatalf("preview request=%+v", request)
	}
}

func TestWorktreePreviewCommandRejectsInvalidUsage(t *testing.T) {
	for _, line := range []string{"/worktrees preview", "/worktrees preview ../outside", "/worktrees preview 1123456789abcdef0123456789abcdef extra"} {
		m := New("", t.TempDir())
		m.ActiveSession = "0123456789abcdef0123456789abcdef"
		m.Composer.SetValue(line)
		updated, command := m.submitComposer()
		if command != nil || !strings.Contains(updated.(Model).Status, "用法") {
			t.Fatalf("invalid preview command accepted: %s status=%q", line, updated.(Model).Status)
		}
	}
}

func TestWorktreePreviewRendersConflictPathsAndInputDigests(t *testing.T) {
	text := formatWorktreeMessages([]conversation.ServerMsg{{
		Type: "worktree",
		Worktree: &workspace.Snapshot{
			ID: "1123456789abcdef0123456789abcdef", Label: "feature", State: workspace.StateKept,
			Generation: 3, ConflictCount: 3, Conflicts: []string{"README.md", "src/app.go"},
			BaselineDigest: "base-digest", FormalDigest: "formal-digest", WorkspaceDigest: "workspace-digest",
		},
	}})
	for _, want := range []string{"冲突 3 项", "README.md", "src/app.go", "另有 1 项未显示", "base-digest", "formal-digest", "workspace-digest"} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview output missing %q: %s", want, text)
		}
	}
}

func TestWorktreeResolutionDialogSendsCompleteUserChoices(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	preview := workspace.Snapshot{
		ID: workspaceID, SessionID: sessionID, PreviewID: "2123456789abcdef0123456789abcdef",
		Generation: 7, ConflictCount: 2, Conflicts: []string{"README.md", "src/app.go"},
		BaselineDigest: "base", FormalDigest: "formal", WorkspaceDigest: "workspace",
	}
	m := New("", t.TempDir())
	m.ActiveSession = sessionID
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	updated, _ := m.handleResult(resultMsg{op: "worktree_preview", msgs: []conversation.ServerMsg{{Type: "worktree", Worktree: &preview}}})
	got := updated.(Model)
	if got.WorktreeDialog == nil || got.pendingDialog() != DialogWorkspace {
		t.Fatal("conflict preview did not open the workspace decision dialog")
	}

	updated, command := got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	got = updated.(Model)
	if command != nil {
		t.Fatal("partial path choice unexpectedly submitted a request")
	}
	updated, command = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	got = updated.(Model)
	if command != nil || got.WorktreeDialog == nil || !strings.Contains(got.Status, "逐路径") {
		t.Fatalf("incomplete choices were submitted: command=%v dialog=%+v status=%q", command != nil, got.WorktreeDialog, got.Status)
	}

	socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "done"})
	got.Socket = socket
	updated, _ = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyDown})
	got = updated.(Model)
	updated, _ = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	got = updated.(Model)
	updated, command = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	got = updated.(Model)
	if command == nil || got.WorktreeDialog != nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatalf("resolution submission changed parent stream state: command=%v pending=%v run=%q cursor=%d", command != nil, got.Pending, got.ActiveRunID, got.LastCursor)
	}
	result := command().(resultMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	request := agentFixtureRequest(t, requests)
	wantChoices := map[string]string{"README.md": workspace.UseWorkspace, "src/app.go": workspace.UseFormal}
	if request.Op != "worktree_resolve" || request.SessionID != sessionID || request.ID != workspaceID || request.WorktreePreviewID != preview.PreviewID || request.WorktreeGeneration != preview.Generation || !reflect.DeepEqual(request.ConflictChoices, wantChoices) || request.RunID != "" || request.Run != nil {
		t.Fatalf("resolution request=%+v", request)
	}
}
