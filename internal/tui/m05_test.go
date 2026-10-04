package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/sessionlog"
)

// --- T10.1 会话选择器过滤与搜索 ---

func TestSessionPickerFiltersByTitle(t *testing.T) {
	m := New("sock", t.TempDir())
	m.Sessions = []sessionlog.SessionInfo{{ID: "a", Title: "封装修复"}, {ID: "b", Title: "布局评审"}}
	m.Navigation = NavigationState{Mode: SessionPickerView, Cursor: 0}
	updated, _ := m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("封")})
	m = updated.(Model)
	if m.Navigation.Filter != "封" {
		t.Fatalf("filter=%q", m.Navigation.Filter)
	}
	view := m.View()
	if !strings.Contains(view, "封装修复") || strings.Contains(view, "布局评审") {
		t.Fatalf("filter did not narrow the picker: %s", view)
	}
	// 过滤无匹配时明确提示；过滤输入本身永不提交。
	updated, _ = m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated.(Model)
	if !strings.Contains(m.View(), "没有匹配的会话") {
		t.Fatalf("empty filter result not shown: %s", m.View())
	}
	updated, _ = m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyBackspace})
	m = updated.(Model)
	if m.Navigation.Filter != "封" {
		t.Fatalf("backspace filter=%q", m.Navigation.Filter)
	}
	updated, cmd := m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.ActiveSession != "a" || cmd == nil {
		t.Fatalf("filtered enter selected %q", m.ActiveSession)
	}
}

func TestSearchCommandOpensResultsAndRestoresSession(t *testing.T) {
	m := New("sock", t.TempDir())
	m.Sessions = []sessionlog.SessionInfo{{ID: "s1"}}
	m.ActiveSession = "s1"
	m.Composer.SetValue("/search 封装")
	updated, cmd := m.submitComposer()
	m = updated.(Model)
	if cmd == nil || !m.Pending {
		t.Fatal("/search did not issue a request")
	}
	if msg := cmd().(resultMsg); msg.op != "session_search" {
		t.Fatalf("op=%s", msg.op)
	}
	updated, _ = m.handleResult(resultMsg{op: "session_search", msgs: []conversation.ServerMsg{{Type: "search", Search: &sessionlog.SearchResult{
		Hits:    []sessionlog.SearchHit{{Session: sessionlog.SessionInfo{ID: "s2", Title: "旧会话"}, Field: "event", Snippet: "…封装…"}},
		Corrupt: []sessionlog.SearchError{{SessionID: "s3", Err: "bad line"}},
	}}}})
	m = updated.(Model)
	if m.Navigation.Mode != SearchResultsView {
		t.Fatalf("mode=%v", m.Navigation.Mode)
	}
	view := m.View()
	for _, want := range []string{"旧会话", "…封装…", "日志损坏"} {
		if !strings.Contains(view, want) {
			t.Fatalf("search view missing %q: %s", want, view)
		}
	}
	// 选择搜索结果后通过 session_load 恢复：transcript 与 agent 上下文同源。
	updated, cmd = m.handleNavigationKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.ActiveSession != "s2" || cmd == nil {
		t.Fatalf("search hit not restored: %q", m.ActiveSession)
	}
	if msg := cmd().(resultMsg); msg.op != "session_load" {
		t.Fatalf("restore op=%s", msg.op)
	}
}

func TestSearchEmptyResultShowsStatus(t *testing.T) {
	m := New("sock", t.TempDir())
	updated, _ := m.handleResult(resultMsg{op: "session_search", msgs: []conversation.ServerMsg{{Type: "search", Search: &sessionlog.SearchResult{}}}})
	m = updated.(Model)
	if m.Navigation.Mode != SearchResultsView || !strings.Contains(m.Status, "没有匹配") {
		t.Fatalf("empty search not explicit: %+v", m)
	}
}

// --- T10.2 输入历史导航 ---

func TestComposerHistoryNavigationRestoresDraft(t *testing.T) {
	root := t.TempDir()
	m := New("sock", root)
	if m.history == nil {
		t.Fatalf("history unavailable: %v", m.historyErr)
	}
	if _, err := m.history.Append("第一条"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.history.Append("第二条"); err != nil {
		t.Fatal(err)
	}
	m.Composer.SetValue("未完成的草稿")
	updated, _ := m.handleChatKey(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = updated.(Model)
	if m.Composer.Value() != "第二条" {
		t.Fatalf("ctrl+p = %q", m.Composer.Value())
	}
	updated, _ = m.handleChatKey(tea.KeyMsg{Type: tea.KeyCtrlP})
	m = updated.(Model)
	if m.Composer.Value() != "第一条" {
		t.Fatalf("second ctrl+p = %q", m.Composer.Value())
	}
	updated, _ = m.handleChatKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = updated.(Model)
	if m.Composer.Value() != "第二条" {
		t.Fatalf("ctrl+n = %q", m.Composer.Value())
	}
	updated, _ = m.handleChatKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = updated.(Model)
	if m.Composer.Value() != "未完成的草稿" {
		t.Fatalf("draft not restored: %q", m.Composer.Value())
	}
	// 导航只是替换输入框内容，不产生任何提交。
	if m.Pending {
		t.Fatal("history navigation submitted a request")
	}
}

func TestSubmitAppendsHistoryAndReportsFailure(t *testing.T) {
	root := t.TempDir()
	m := New("sock", root)
	m.ActiveSession = "s1"
	m.Composer.SetValue("/search 关键词")
	updated, _ := m.submitComposer()
	m = updated.(Model)
	entries, err := m.history.List()
	if err != nil || len(entries) != 1 || entries[0].Text != "/search 关键词" {
		t.Fatalf("history after submit = %+v err=%v", entries, err)
	}
	// 损坏历史文件后提交：消息仍然发出，但历史保存失败必须可见。
	path := filepath.Join(root, ".stable", "input-history.jsonl")
	if err := os.WriteFile(path, []byte("{corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m.Composer.SetValue("/search 再次")
	updated, cmd := m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("submission blocked by history failure")
	}
	if !strings.Contains(m.Status, "输入历史未保存") {
		t.Fatalf("history failure not visible: %q", m.Status)
	}
}

// --- T10.3 transcript 恢复显示 ---

func TestTranscriptRendersRecoveryAndOwnershipEvents(t *testing.T) {
	now := time.Now().UTC()
	events := []sessionlog.Event{
		{Seq: 1, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: "改一下"}},
		{Seq: 2, Type: sessionlog.EventToolCall, Data: sessionlog.ToolCall{CallID: "c1", Name: "write_file", Input: map[string]string{"file_path": "a.txt"}}},
		{Seq: 3, Type: sessionlog.EventToolResult, Data: sessionlog.ToolResult{CallID: "c1", Result: "ok"}},
		{Seq: 4, Type: sessionlog.EventToolCall, Data: sessionlog.ToolCall{CallID: "c2", Name: "edit_file"}},
		{Seq: 5, Type: sessionlog.EventBoundary, Data: sessionlog.Boundary{FromSeq: 1, ToSeq: 2, Summary: "较早内容摘要"}},
		{Seq: 6, Type: sessionlog.EventSnapshot, Data: sessionlog.SnapshotRef{SnapshotID: "snap1", CandidateID: "cand", RunID: "r1", Label: "pre:write_file", Digest: "abcdef1234567890", CreatedAt: now}},
		{Seq: 7, Type: sessionlog.EventRewind, Data: sessionlog.RewindRecord{SnapshotID: "snap1", CandidateID: "cand", Status: sessionlog.RewindPending, CreatedAt: now}},
		{Seq: 8, Type: sessionlog.EventRewind, Data: sessionlog.RewindRecord{SnapshotID: "snap1", CandidateID: "cand", Status: sessionlog.RewindCompleted, CreatedAt: now}},
		{Seq: 9, Type: sessionlog.EventRewind, Data: sessionlog.RewindRecord{SnapshotID: "snap2", CandidateID: "cand", Status: sessionlog.RewindFailed, Error: "exchange failed", CreatedAt: now}},
		{Seq: 10, Type: sessionlog.EventQuestion, Data: sessionlog.PendingQuestion{QuestionID: "q1", WorkRef: "session/s", SessionID: "s", RunID: "r1", Prompt: "继续吗？", CreatedAt: now, Status: sessionlog.QuestionPending}},
		{Seq: 11, Type: sessionlog.EventReply, Data: sessionlog.QuestionReply{QuestionID: "q1", ReplyText: "继续", RepliedAt: now}},
	}
	view := projectTranscript(events, 90, false)
	for _, want := range []string{
		"工具调用", "write_file", "工具结果",
		"工具调用（未配对，可能已中断）", "edit_file",
		"上下文压缩", "较早内容摘要",
		"快照", "pre:write_file", "abcdef123456",
		"回滚", "待处理", "成功", "失败", "exchange failed",
		"待答问题", "继续吗？", "（已回答）", "问题回复",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("transcript missing %q:\n%s", want, view)
		}
	}
}

// --- T11.1/2 快照展示与 rewind 确认 ---

func TestReviewShowsSnapshotsAndRewindFlow(t *testing.T) {
	review := &candidate.Review{ID: "r", CandidateID: "cand", Digest: "preview", CandidateDigest: "cd", FormalDigest: "fd"}
	now := time.Now().UTC()
	snaps := []sessionlog.SnapshotRef{
		{SnapshotID: "s1", CandidateID: "cand", RunID: "r1", Label: "pre:write_file", Digest: "111111111111aaaa", CreatedAt: now},
		{SnapshotID: "s2", CandidateID: "cand", RunID: "r1", Label: "post:write_file", Digest: "222222222222bbbb", CreatedAt: now},
	}
	out := renderReview(*review, map[string]bool{}, 0, snaps, false, 0, false, false, "", "", 120)
	for _, want := range []string{"pre:write_file", "post:write_file", "111111111111", "运行 r1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("review snapshots missing %q:\n%s", want, out)
		}
	}

	m := New("sock", t.TempDir())
	m.Review, m.ReviewConfirmed = review, map[string]bool{}
	m.ReviewSnapshots = snaps
	// 活动运行时禁用并说明原因。
	m.ActiveRunID = "busy"
	updated, cmd := m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = updated.(Model)
	if cmd != nil || m.RewindPick || !strings.Contains(m.Status, "活动运行") {
		t.Fatalf("rewind not blocked during active run: %+v", m)
	}
	m.ActiveRunID = ""
	updated, _ = m.handleReviewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = updated.(Model)
	if !m.RewindPick {
		t.Fatal("rewind picker did not open")
	}
	updated, _ = m.handleRewindPickKey(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.RewindCursor != 1 {
		t.Fatalf("cursor=%d", m.RewindCursor)
	}
	// 两步确认：第一次 Enter 只是锁定目标。
	updated, cmd = m.handleRewindPickKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd != nil || !m.RewindArmed {
		t.Fatal("first enter should arm, not send")
	}
	updated, cmd = m.handleRewindPickKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("confirmed rewind did not send")
	}
	if msg := cmd().(resultMsg); msg.op != "snapshot_rewind" {
		t.Fatalf("rewind op=%s", msg.op)
	}
}

func TestRewindReceiptRefreshesReview(t *testing.T) {
	review := &candidate.Review{ID: "r", CandidateID: "cand", Digest: "preview", CandidateDigest: "cd", FormalDigest: "fd"}
	m := New("sock", t.TempDir())
	m.Review, m.ReviewConfirmed = review, map[string]bool{}
	updated, cmd := m.handleResult(resultMsg{op: "snapshot_rewind", msgs: []conversation.ServerMsg{{Type: "rewind", Rewind: &sessionlog.RewindRecord{SnapshotID: "s1", CandidateID: "cand", Status: sessionlog.RewindCompleted}}}})
	m = updated.(Model)
	if !strings.Contains(m.Status, "已回滚到快照 s1") {
		t.Fatalf("receipt missing: %q", m.Status)
	}
	if cmd == nil {
		t.Fatal("review not refreshed after rewind")
	}
	if msg := cmd().(resultMsg); msg.op != "review_get" {
		t.Fatalf("refresh op=%s", msg.op)
	}
}

func TestReviewGetAlsoListsSnapshots(t *testing.T) {
	review := &candidate.Review{ID: "r", CandidateID: "cand", Digest: "preview"}
	m := New("sock", t.TempDir())
	m.ActiveSession = "sess"
	updated, cmd := m.handleResult(resultMsg{op: "review_get", msgs: []conversation.ServerMsg{{Type: "review", Review: review}}})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("snapshot list not requested with review")
	}
	if msg := cmd().(resultMsg); msg.op != "snapshot_list" {
		t.Fatalf("follow-up op=%s", msg.op)
	}
	updated, _ = m.handleResult(resultMsg{op: "snapshot_list", msgs: []conversation.ServerMsg{{Type: "snapshots", Snapshots: []sessionlog.SnapshotRef{{SnapshotID: "s1", CandidateID: "cand", Digest: "d", CreatedAt: time.Now().UTC()}}}}})
	m = updated.(Model)
	if len(m.ReviewSnapshots) != 1 {
		t.Fatalf("snapshots=%+v", m.ReviewSnapshots)
	}
}

// --- T11.3 /say 与 /reply 交互 ---

func TestSayQueuesWithExplicitStatus(t *testing.T) {
	m := New("sock", t.TempDir())
	m.Goals = []core.Goal{{ID: "g1", Objective: "obj"}}
	m.Composer.SetValue("/say 继续下一步")
	updated, cmd := m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("/say did not submit")
	}
	if msg := cmd().(resultMsg); msg.op != "say" {
		t.Fatalf("op=%s", msg.op)
	}
	updated, _ = m.handleResult(resultMsg{op: "say", msgs: []conversation.ServerMsg{{Type: "message"}}})
	m = updated.(Model)
	if !strings.Contains(m.Status, "已排队") {
		t.Fatalf("queued status missing: %q", m.Status)
	}

	m.Goals = nil
	m.Composer.SetValue("/say 再来")
	updated, cmd = m.submitComposer()
	m = updated.(Model)
	if cmd != nil || !strings.Contains(m.Status, "选择目标") {
		t.Fatalf("/say without goal not guided: %q", m.Status)
	}
}

func TestReplyRequiresPendingQuestion(t *testing.T) {
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	m.Composer.SetValue("/reply 继续")
	updated, cmd := m.submitComposer()
	m = updated.(Model)
	if cmd != nil || !strings.Contains(m.Status, "没有待回答的问题") {
		t.Fatalf("/reply without question not refused: %q", m.Status)
	}

	m.Questions = []sessionlog.PendingQuestion{
		{QuestionID: "q-old", Status: sessionlog.QuestionReplied},
		{QuestionID: "q-new", Status: sessionlog.QuestionPending, Prompt: "继续吗？"},
	}
	m.Composer.SetValue("/reply 继续")
	updated, cmd = m.submitComposer()
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("/reply with pending question did not submit")
	}
	if msg := cmd().(resultMsg); msg.op != "reply" {
		t.Fatalf("op=%s", msg.op)
	}
	updated, cmd = m.handleResult(resultMsg{op: "reply", msgs: []conversation.ServerMsg{{Type: "reply", Reply: &sessionlog.QuestionReply{QuestionID: "q-new", ReplyText: "继续"}}}})
	m = updated.(Model)
	if !strings.Contains(m.Status, "答复已记录") {
		t.Fatalf("reply status=%q", m.Status)
	}
	if cmd == nil {
		t.Fatal("question list not refreshed after reply")
	}
	if msg := cmd().(resultMsg); msg.op != "question_list" {
		t.Fatalf("refresh op=%s", msg.op)
	}
}

func TestSessionLoadAlsoListsQuestions(t *testing.T) {
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	_, cmd := m.handleResult(resultMsg{op: "session_load", msgs: []conversation.ServerMsg{{Type: "transcript", Transcript: &sessionlog.Transcript{}}}})
	if cmd == nil {
		t.Fatal("session load did not batch follow-ups")
	}
	updated, _ := m.handleResult(resultMsg{op: "question_list", msgs: []conversation.ServerMsg{{Type: "questions", Questions: []sessionlog.PendingQuestion{{QuestionID: "q1", Status: sessionlog.QuestionPending}}}}})
	m = updated.(Model)
	if _, ok := m.pendingQuestion(); !ok {
		t.Fatalf("questions not stored: %+v", m.Questions)
	}
}
