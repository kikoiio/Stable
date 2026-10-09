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

func TestWorktreeLateResponseFromPreviousSessionIsIgnored(t *testing.T) {
	const oldSession = "0123456789abcdef0123456789abcdef"
	const activeSession = "2123456789abcdef0123456789abcdef"
	m := New("", t.TempDir())
	m.ActiveSession = activeSession
	m.Worktrees = []workspace.Snapshot{{ID: "3123456789abcdef0123456789abcdef", SessionID: activeSession, Label: "current"}}
	m.WorktreeDialog = &worktreeDecisionDialog{Mode: "resolve", Snapshot: workspace.Snapshot{
		ID: "4123456789abcdef0123456789abcdef", SessionID: oldSession, Conflicts: []string{"old/private/path"},
	}}
	m.worktreeNextPage = &worktreePageRequest{sessionID: oldSession, workKind: "session", workspaceID: "4123456789abcdef0123456789abcdef", after: "old/private/path"}

	updated, command := m.handleResult(resultMsg{
		op: "worktree_resolve", sessionID: oldSession,
		msgs: []conversation.ServerMsg{{Type: "worktree", Worktree: &workspace.Snapshot{
			ID: "4123456789abcdef0123456789abcdef", SessionID: oldSession, Label: "old", Conflicts: []string{"old/private/path"},
		}}},
	})
	got := updated.(Model)
	if command != nil {
		t.Fatal("stale workspace response started a continuation request")
	}
	if len(got.Worktrees) != 1 || got.Worktrees[0].SessionID != activeSession || got.Worktrees[0].Label != "current" {
		t.Fatalf("stale response changed the active session workspace list: %+v", got.Worktrees)
	}
	if got.WorktreeDialog != nil {
		t.Fatalf("stale conflict paths remained visible in the active session: %+v", got.WorktreeDialog)
	}
	if got.worktreeNextPage == nil || got.worktreeNextPage.sessionID != oldSession {
		t.Fatalf("stale pagination state was lost or rebound: %+v", got.worktreeNextPage)
	}
}

func TestWorktreeLateResponseFromPreviousGoalItemIsIgnored(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	m := New("", t.TempDir())
	m.ActiveSession = sessionID
	m.WorktreeScopeSessionID, m.WorktreeGoalID, m.WorktreeWorkItemID = sessionID, "goal-current", "item-current"
	m.Worktrees = []workspace.Snapshot{{ID: "3123456789abcdef0123456789abcdef", SessionID: sessionID, Label: "current"}}
	updated, command := m.handleResult(resultMsg{
		op: "worktree_get", sessionID: sessionID, workKind: "goal", goalID: "goal-old", workItemID: "item-old",
		msgs: []conversation.ServerMsg{{Type: "worktree", Worktree: &workspace.Snapshot{
			ID: "4123456789abcdef0123456789abcdef", SessionID: sessionID, Label: "old-goal", Conflicts: []string{"private/path"},
		}}},
	})
	got := updated.(Model)
	if command != nil {
		t.Fatal("stale Goal reply started a follow-up request")
	}
	if len(got.Worktrees) != 1 || got.Worktrees[0].Label != "current" {
		t.Fatalf("previous Goal/WorkItem reply changed active worktree list: %+v", got.Worktrees)
	}
}

func TestWorktreeCommandsDispatchSessionScopedLifecycleRequests(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	tests := []struct {
		line  string
		op    string
		id    string
		runID string
		text  string
	}{
		{line: "/worktrees", op: "worktree_list"},
		{line: "/worktrees get " + workspaceID, op: "worktree_get", id: workspaceID},
		{line: "/worktrees enter " + workspaceID, op: "worktree_enter", id: workspaceID},
		{line: "/worktrees exit", op: "worktree_exit"},
		{line: "/worktrees keep " + workspaceID, op: "worktree_keep", id: workspaceID},
		{line: "/worktrees export " + workspaceID, op: "worktree_export", id: workspaceID},
		{line: "/worktrees resolve " + workspaceID, op: "worktree_preview", id: workspaceID},
		{line: "/worktrees remove " + workspaceID, op: "worktree_remove", id: workspaceID},
		{line: "/worktrees discard " + workspaceID, op: "worktree_discard_preview", id: workspaceID},
		{line: "/worktrees create review space", op: "worktree_create", runID: "parent-run", text: "review space"},
	}
	for _, test := range tests {
		t.Run(test.op, func(t *testing.T) {
			m := New("", t.TempDir())
			m.ActiveSession = sessionID
			m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
			parentStream := &conversation.StreamClient{}
			m.stream = parentStream
			socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "worktree"})
			m.Socket = socket
			m.Composer.SetValue(test.line)

			updated, command := m.submitComposer()
			got := updated.(Model)
			if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
				t.Fatalf("command changed parent run state: pending=%v run=%q cursor=%d", got.Pending, got.ActiveRunID, got.LastCursor)
			}
			if result := command().(resultMsg); result.err != nil {
				t.Fatal(result.err)
			}
			request := agentFixtureRequest(t, requests)
			if request.Op != test.op || request.SessionID != sessionID || request.WorkKind != "session" || request.ID != test.id || request.RunID != test.runID || request.Text != test.text || request.Run != nil {
				t.Fatalf("request=%+v want op=%s id=%s run=%s text=%q", request, test.op, test.id, test.runID, test.text)
			}
		})
	}
}

func TestWorktreeScopeSelectionRequiresGoalAndWorkItemAndCanClear(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	m := New("", t.TempDir())
	m.ActiveSession = sessionID

	updated, command := submitWorktreeLine(t, m, "/worktrees scope goal goal-one")
	if command != nil || updated.WorktreeGoalID != "" || updated.WorktreeWorkItemID != "" {
		t.Fatalf("incomplete Goal scope changed selection: model=%+v command=%v", updated, command != nil)
	}
	updated, command = submitWorktreeLine(t, updated, "/worktrees scope goal goal-one item-one")
	if command != nil || updated.WorktreeScopeSessionID != sessionID || updated.WorktreeGoalID != "goal-one" || updated.WorktreeWorkItemID != "item-one" {
		t.Fatalf("paired Goal scope not selected: model=%+v command=%v", updated, command != nil)
	}
	if !strings.Contains(updated.Status, "goal-one") || !strings.Contains(updated.Status, "item-one") {
		t.Fatalf("selected scope not shown to user: %q", updated.Status)
	}
	updated, command = submitWorktreeLine(t, updated, "/worktrees scope session")
	if command != nil || updated.WorktreeScopeSessionID != "" || updated.WorktreeGoalID != "" || updated.WorktreeWorkItemID != "" {
		t.Fatalf("Session scope did not clear Goal selection: model=%+v command=%v", updated, command != nil)
	}
}

func TestWorktreeCommandsDispatchSelectedGoalWorkItemScope(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	tests := []struct {
		line  string
		op    string
		id    string
		runID string
		text  string
	}{
		{line: "/worktrees", op: "worktree_list"},
		{line: "/worktrees get " + workspaceID, op: "worktree_get", id: workspaceID},
		{line: "/worktrees enter " + workspaceID, op: "worktree_enter", id: workspaceID},
		{line: "/worktrees exit", op: "worktree_exit"},
		{line: "/worktrees keep " + workspaceID, op: "worktree_keep", id: workspaceID},
		{line: "/worktrees export " + workspaceID, op: "worktree_export", id: workspaceID},
		{line: "/worktrees resolve " + workspaceID, op: "worktree_preview", id: workspaceID},
		{line: "/worktrees remove " + workspaceID, op: "worktree_remove", id: workspaceID},
		{line: "/worktrees discard " + workspaceID, op: "worktree_discard_preview", id: workspaceID},
		{line: "/worktrees create review space", op: "worktree_create", runID: "goal-run", text: "review space"},
	}
	for _, test := range tests {
		t.Run(test.op, func(t *testing.T) {
			m := New("", t.TempDir())
			m.ActiveSession, m.ActiveRunID = sessionID, "goal-run"
			updated, command := submitWorktreeLine(t, m, "/worktrees scope goal goal-one item-one")
			if command != nil {
				t.Fatal("scope selection unexpectedly made a socket request")
			}
			socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "worktree"})
			updated.Socket = socket
			updated, command = submitWorktreeLine(t, updated, test.line)
			if command == nil {
				t.Fatal("workspace command did not dispatch")
			}
			if result := command().(resultMsg); result.err != nil {
				t.Fatal(result.err)
			}
			request := agentFixtureRequest(t, requests)
			if request.Op != test.op || request.SessionID != sessionID || request.WorkKind != "goal" || request.GoalID != "goal-one" || request.WorkItemID != "item-one" || request.ID != test.id || request.RunID != test.runID || request.Text != test.text {
				t.Fatalf("request=%+v want Goal scope operation %s", request, test.op)
			}
		})
	}
}

func submitWorktreeLine(t *testing.T, model Model, line string) (Model, tea.Cmd) {
	t.Helper()
	model.Composer.SetValue(line)
	updated, command := model.submitComposer()
	got, ok := updated.(Model)
	if !ok {
		t.Fatalf("submit %q returned %T", line, updated)
	}
	return got, command
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
	updated, _ := m.handleResult(resultMsg{op: "worktree_preview", sessionID: sessionID, workKind: "session", msgs: []conversation.ServerMsg{{Type: "worktree", Worktree: &preview}}})
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

func TestWorktreeDiscardDialogRequiresArmedUserConfirmation(t *testing.T) {
	const sessionID = "0123456789abcdef0123456789abcdef"
	const workspaceID = "1123456789abcdef0123456789abcdef"
	preview := workspace.Snapshot{
		ID: workspaceID, SessionID: sessionID, Generation: 9,
		DiscardID: "2123456789abcdef0123456789abcdef", DiscardDigest: "discard-digest",
		ChangedFiles: 2, DiscardPaths: []string{"README.md", "src/app.go"},
	}
	m := New("", t.TempDir())
	m.ActiveSession = sessionID
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	updated, _ := m.handleResult(resultMsg{op: "worktree_discard_preview", sessionID: sessionID, workKind: "session", msgs: []conversation.ServerMsg{{Type: "worktree", Worktree: &preview}}})
	got := updated.(Model)
	if got.WorktreeDialog == nil || got.WorktreeDialog.Mode != "discard" {
		t.Fatal("discard preview did not open its user decision dialog")
	}

	updated, command := got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	got = updated.(Model)
	if command != nil || got.WorktreeDialog == nil {
		t.Fatal("discard was submitted without arming the explicit user confirmation")
	}
	updated, command = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	got = updated.(Model)
	if command != nil || got.WorktreeDialog == nil || !got.WorktreeDialog.Armed {
		t.Fatal("discard did not require a separate confirmation step")
	}

	socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "done"})
	got.Socket = socket
	updated, command = got.handleWorktreeDecisionKey(tea.KeyMsg{Type: tea.KeyEnter})
	got = updated.(Model)
	if command == nil || got.WorktreeDialog != nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatalf("confirmed discard changed parent stream state: command=%v pending=%v run=%q cursor=%d", command != nil, got.Pending, got.ActiveRunID, got.LastCursor)
	}
	result := command().(resultMsg)
	if result.err != nil {
		t.Fatal(result.err)
	}
	request := agentFixtureRequest(t, requests)
	if request.Op != "worktree_discard" || request.SessionID != sessionID || request.ID != workspaceID || request.DecisionID != preview.DiscardID || request.PreviewDigest != preview.DiscardDigest || request.WorktreeGeneration != preview.Generation || request.RunID != "" {
		t.Fatalf("discard request=%+v", request)
	}
}
