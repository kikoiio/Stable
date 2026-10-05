package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func keyEnter() tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeyEnter} }
func keyEsc() tea.KeyMsg           { return tea.KeyMsg{Type: tea.KeyEsc} }
func keyDown() tea.KeyMsg          { return tea.KeyMsg{Type: tea.KeyDown} }
func keySpace() tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeySpace} }
func keyBackspace() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyBackspace} }
func cmdOp(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	msg := cmd()
	result, ok := msg.(resultMsg)
	if !ok {
		t.Fatalf("expected resultMsg, got %T", msg)
	}
	return result.op
}

// newDialogModel builds a model with one live pending decision of every kind:
// a permission approval, a pending question, a plan approval, a review, and a
// live goal proposal — the full AC7 queue.
func newDialogModel(t *testing.T) Model {
	t.Helper()
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	m.Approvals = []permission.ApprovalPrompt{{ID: "a1", Name: "run-check"}}
	question := sessionlog.PendingQuestion{
		QuestionID: "q1", WorkRef: "w", SessionID: "s1", RunID: "r1",
		Prompt: "[配置] 使用哪个镜像仓库？\n  - 内部源：速度快\n  - 官方源：较慢",
		Status: sessionlog.QuestionPending,
	}
	m.Questions = []sessionlog.PendingQuestion{question}
	m.liveQuestions = map[string]bool{"q1": true}
	m.PlanApprovals = []conversation.PlanApprovalRef{{ID: "pa1", SessionID: "s1", RunID: "r1", PlanPath: ".stable/plans/s1.md"}}
	m.Review = &candidate.Review{ID: "rv", CandidateID: "cand"}
	m.Proposals = []core.CriteriaProposal{{ID: "p1", Status: core.ProposalPending, RawText: "把构建搬到容器里", Criteria: []core.Criterion{{ID: "c1", Kind: core.CriterionKindERCClean, Payload: json.RawMessage(`{"net":"N1"}`)}}}}
	m.liveProposals = map[string]bool{"p1": true}
	return m
}

// --- 弹层优先级队列（AC7）与 Esc 层级降落 ---

func TestPendingDialogPriorityOrderAndFirstKeyGoesToApproval(t *testing.T) {
	m := newDialogModel(t)
	if got := m.pendingDialog(); got != DialogApproval {
		t.Fatalf("first pending dialog = %v, want approval", got)
	}
	if view := m.View(); !strings.Contains(view, "需要授权") {
		t.Fatalf("approval dialog not rendered first:\n%s", view)
	}
	// 第一个按键必须归审批层处理，而不是透传到下面的弹层。
	updated, cmd := m.Update(keyRunes("1"))
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "approval_resolve" {
		t.Fatalf("first key handled by %q, want approval_resolve", op)
	}
	if _, ok := m.popupQuestion(); !ok {
		t.Fatal("question dialog must still be pending behind the approval")
	}
}

func TestEscFallsThroughTheDialogLadder(t *testing.T) {
	m := newDialogModel(t)
	// 审批保持等待（既有行为：Esc 不取消授权请求）。
	if _, cmd := m.handleApprovalKey(keyEsc()); cmd != nil || len(m.Approvals) != 1 {
		t.Fatal("approval esc must stay in place")
	}
	m.Approvals = nil // 服务端解决后队列自动下落。
	if got := m.pendingDialog(); got != DialogQuestion {
		t.Fatalf("after approval the question should surface, got %v", got)
	}
	updated, cmd := m.Update(keyEsc())
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("question esc must not send anything")
	}
	if m.Questions[0].Status != sessionlog.QuestionPending {
		t.Fatal("esc must leave the question pending")
	}
	if got := m.pendingDialog(); got != DialogPlan {
		t.Fatalf("deferred question must drop to the plan dialog, got %v", got)
	}
	if view := m.View(); !strings.Contains(view, "计划审批") || !strings.Contains(view, ".stable/plans/s1.md") {
		t.Fatalf("plan dialog not rendered:\n%s", view)
	}
	updated, cmd = m.Update(keyEsc())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "plan_resolve" {
		t.Fatalf("plan esc op = %q, want plan_resolve (cancel)", op)
	}
	if !m.Pending {
		t.Fatal("plan cancel should mark the model pending")
	}
	m.PlanApprovals = nil
	if got := m.pendingDialog(); got != DialogReview {
		t.Fatalf("resolved plan must drop to the review, got %v", got)
	}
	updated, cmd = m.Update(keyEsc())
	m = updated.(Model)
	if cmd != nil || m.Review != nil {
		t.Fatal("review esc must close the review and nothing else")
	}
	if got := m.pendingDialog(); got != DialogProposal {
		t.Fatalf("closed review must drop to the proposal, got %v", got)
	}
	if view := m.View(); !strings.Contains(view, "目标提案 p1") || !strings.Contains(view, "把构建搬到容器里") || !strings.Contains(view, "kicad.erc_clean") {
		t.Fatalf("proposal dialog not rendered:\n%s", view)
	}
	updated, cmd = m.Update(keyEsc())
	m = updated.(Model)
	if cmd != nil {
		t.Fatal("proposal esc must not send anything")
	}
	if got := m.pendingDialog(); got != DialogNone {
		t.Fatalf("deferred proposal must empty the queue, got %v", got)
	}
	if m.Proposals[0].ID != "p1" || m.Proposals[0].Status != core.ProposalPending {
		t.Fatalf("deferred proposal state changed: %+v", m.Proposals[0])
	}
}

// --- 提问弹层 ---

func TestQuestionDialogSingleSelectSubmitsLabel(t *testing.T) {
	m := newDialogModel(t)
	updated, cmd := m.handleQuestionKey(keyRunes("1"))
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "reply" {
		t.Fatalf("single-select number op = %q, want reply", op)
	}
	if !m.Pending {
		t.Fatal("reply submission should mark the model pending")
	}
}

func TestQuestionDialogMultiSelectTogglesThenSubmitsJoinedLabels(t *testing.T) {
	prompt := "[配置] 开启哪些功能？\n  - 甲：第一个\n  - 乙：第二个\n  (可多选)"
	_, _, options, multi := parseQuestionSpec(prompt)
	if !multi || len(options) != 2 {
		t.Fatalf("parse = %+v multi=%v", options, multi)
	}
	m := newDialogModel(t)
	m.Questions = []sessionlog.PendingQuestion{{QuestionID: "qm", SessionID: "s1", Prompt: prompt, Status: sessionlog.QuestionPending}}
	m.liveQuestions = map[string]bool{"qm": true}
	for _, k := range []tea.KeyMsg{keyRunes("2"), keyRunes("1")} {
		updated, cmd := m.handleQuestionKey(k)
		m = updated.(Model)
		if cmd != nil {
			t.Fatal("multi-select number keys only toggle, never submit")
		}
	}
	if got := questionReplyText(options, m.QuestionPicked); got != "甲,乙" {
		t.Fatalf("joined answer = %q, want 甲,乙", got)
	}
	updated, cmd := m.handleQuestionKey(keyEnter())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "reply" {
		t.Fatalf("multi enter op = %q", op)
	}
}

func TestQuestionDialogOtherFreeInput(t *testing.T) {
	m := newDialogModel(t)
	// 视图断言只关心提问层，先清掉更高优先级的待决策。
	m.Approvals, m.Review, m.PlanApprovals, m.liveProposals = nil, nil, nil, nil
	updated, cmd := m.handleQuestionKey(keyRunes("o"))
	m = updated.(Model)
	if cmd != nil || !m.QuestionOther {
		t.Fatalf("o did not open the free input: cmd=%v other=%v", cmd, m.QuestionOther)
	}
	if view := m.View(); !strings.Contains(view, "其他答复：") {
		t.Fatalf("free input line missing:\n%s", view)
	}
	updated, _ = m.handleQuestionKey(keyRunes("就用内部源"))
	m = updated.(Model)
	updated, cmd = m.handleQuestionKey(keyEnter())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "reply" {
		t.Fatalf("other free input submit op = %q", op)
	}
	// 空自由输入被拒绝，不发请求（通过按键进入自由输入，弹层状态才归属当前问题）。
	m2 := newDialogModel(t)
	m2.Approvals, m2.Review, m2.PlanApprovals, m2.liveProposals = nil, nil, nil, nil
	updated, _ = m2.handleQuestionKey(keyRunes("o"))
	m2 = updated.(Model)
	updated, cmd = m2.handleQuestionKey(keyEnter())
	m2 = updated.(Model)
	if cmd != nil || !strings.Contains(m2.Status, "非空") {
		t.Fatalf("empty free input not refused: cmd=%v status=%q", cmd, m2.Status)
	}
	// Esc 退回选项列表，退格编辑自由输入后再提交。
	updated, _ = m2.handleQuestionKey(keyEsc())
	m2 = updated.(Model)
	if m2.QuestionOther {
		t.Fatal("esc must leave the free input mode")
	}
	updated, _ = m2.handleQuestionKey(keyRunes("o"))
	m2 = updated.(Model)
	updated, _ = m2.handleQuestionKey(keyRunes("ab"))
	m2 = updated.(Model)
	updated, _ = m2.handleQuestionKey(keyBackspace())
	m2 = updated.(Model)
	updated, cmd = m2.handleQuestionKey(keyEnter())
	m2 = updated.(Model)
	if op := cmdOp(t, cmd); op != "reply" {
		t.Fatalf("edited free input submit op = %q", op)
	}
}

func TestQuestionDialogReplyResultClosesDialog(t *testing.T) {
	m := newDialogModel(t)
	updated, _ := m.handleQuestionKey(keyRunes("1"))
	m = updated.(Model)
	updated, cmd := m.handleResult(resultMsg{op: "reply", msgs: []conversation.ServerMsg{{Type: "reply", Reply: &sessionlog.QuestionReply{QuestionID: "q1", ReplyText: "内部源"}}}})
	m = updated.(Model)
	if !strings.Contains(m.Status, "答复已记录") {
		t.Fatalf("reply status missing: %q", m.Status)
	}
	if op := cmdOp(t, cmd); op != "question_list" {
		t.Fatalf("reply refresh op = %q", op)
	}
	if got := m.pendingDialog(); got == DialogQuestion {
		t.Fatal("answered question must leave the dialog queue")
	}
}

// --- 计划审批弹层 ---

func TestPlanDialogChoicesSendPlanResolve(t *testing.T) {
	if got := planResolveRequest("s1", conversation.PlanResolveFeedback, "改成 X").Text; got != "改成 X" {
		t.Fatalf("feedback text lost: %q", got)
	}
	if got := planResolveRequest("s1", conversation.PlanResolveAuto, "").ApprovalChoice; got != conversation.PlanResolveAuto {
		t.Fatalf("auto choice = %q", got)
	}
	m := newDialogModel(t)
	m.Approvals, m.Review, m.liveQuestions = nil, nil, nil
	m.liveProposals = nil
	updated, cmd := m.handlePlanKey(keyEnter())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "plan_resolve" {
		t.Fatalf("auto enter op = %q", op)
	}
	updated, cmd = m.handlePlanKey(keyDown())
	m = updated.(Model)
	updated, cmd = m.handlePlanKey(keyEnter())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "plan_resolve" {
		t.Fatalf("manual enter op = %q", op)
	}
	if m.SelectedPlan != 1 {
		t.Fatalf("cursor = %d, want 1", m.SelectedPlan)
	}
	updated, cmd = m.handlePlanKey(keyDown())
	m = updated.(Model)
	updated, cmd = m.handlePlanKey(keyEnter())
	m = updated.(Model)
	if cmd != nil || !strings.Contains(m.Status, "反馈") {
		t.Fatalf("empty feedback must be refused: cmd=%v status=%q", cmd, m.Status)
	}
	if view := m.View(); !strings.Contains(view, "反馈：<输入纠偏文本>") {
		t.Fatalf("feedback input line missing:\n%s", view)
	}
	updated, _ = m.handlePlanKey(keyRunes("请改成手动模式"))
	m = updated.(Model)
	if m.PlanFeedback != "请改成手动模式" {
		t.Fatalf("feedback not captured: %q", m.PlanFeedback)
	}
	updated, cmd = m.handlePlanKey(keyEnter())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "plan_resolve" {
		t.Fatalf("feedback enter op = %q", op)
	}
	// Esc 直接取消审批。
	m.PlanFeedback = ""
	updated, cmd = m.handlePlanKey(keyEsc())
	m = updated.(Model)
	if op := cmdOp(t, cmd); op != "plan_resolve" {
		t.Fatalf("esc op = %q, want plan_resolve cancel", op)
	}
}

func TestPlanResolveResultClearsDialogAndAppliesState(t *testing.T) {
	m := newDialogModel(t)
	m.Approvals, m.Review, m.liveQuestions, m.liveProposals = nil, nil, nil, nil
	updated, _ := m.handleResult(resultMsg{op: "plan_resolve", msgs: []conversation.ServerMsg{
		{Type: "plan_state", PlanState: &conversation.PlanState{Mode: sessionlog.PlanModeDefault, ExecutionMode: conversation.PlanExecutionAcceptEdits}},
	}})
	m = updated.(Model)
	if len(m.PlanApprovals) != 0 {
		t.Fatalf("plan approvals not cleared: %+v", m.PlanApprovals)
	}
	if m.Plan == nil || m.Plan.ExecutionMode != conversation.PlanExecutionAcceptEdits {
		t.Fatalf("plan state not applied: %+v", m.Plan)
	}
	if got := m.pendingDialog(); got != DialogNone {
		t.Fatalf("resolved plan dialog still pending: %v", got)
	}
}

// --- 提案弹层 ---

func TestProposalDialogConfirmAndRejectReuseExistingOps(t *testing.T) {
	for _, tc := range []struct {
		key, op string
	}{{"c", "confirm"}, {"r", "reject"}} {
		m := newDialogModel(t)
		m.Approvals, m.Review, m.liveQuestions, m.PlanApprovals = nil, nil, nil, nil
		updated, cmd := m.handleProposalKey(keyRunes(tc.key))
		m = updated.(Model)
		if op := cmdOp(t, cmd); op != tc.op {
			t.Fatalf("key %q op = %q, want %q", tc.key, op, tc.op)
		}
		if got := m.pendingDialog(); got == DialogProposal {
			t.Fatalf("decided proposal %q still pops", tc.key)
		}
		if m.Proposals[0].ID != "p1" || m.Proposals[0].Status != core.ProposalPending {
			t.Fatalf("dialog decision changed proposal state: %+v", m.Proposals[0])
		}
	}
}

// --- todo 快照只存状态（transcript 由事件投影渲染） ---

func TestTodoSnapshotStoredFromPushAndResult(t *testing.T) {
	m := New("sock", t.TempDir())
	tasks := []sessionlog.TaskSnapshot{{ID: "t1", Subject: "搭骨架", Status: "in_progress"}}
	m.applyRunMessage(conversation.ServerMsg{Type: "todo", Tasks: tasks})
	if len(m.Todos) != 1 || m.Todos[0].Subject != "搭骨架" {
		t.Fatalf("todo push not stored: %+v", m.Todos)
	}
	updated, _ := m.handleResult(resultMsg{op: "session_load", msgs: []conversation.ServerMsg{{Type: "todo", Tasks: []sessionlog.TaskSnapshot{{ID: "t2", Subject: "补测试", Status: "pending"}}}}})
	m = updated.(Model)
	if len(m.Todos) != 1 || m.Todos[0].ID != "t2" {
		t.Fatalf("todo result not stored: %+v", m.Todos)
	}
}

// --- 状态栏计划模式 ---

func TestPlanModeShownInStatusBarFromRestoreAndPush(t *testing.T) {
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	if m.planModeActive() || strings.Contains(m.View(), "计划模式") {
		t.Fatal("default session must not show plan mode")
	}
	updated, _ := m.handleResult(resultMsg{op: "session_load", msgs: []conversation.ServerMsg{{
		Type: "transcript",
		Transcript: &sessionlog.Transcript{Events: []sessionlog.Event{{
			Seq:  1,
			Type: sessionlog.EventProposal,
			Data: core.CriteriaProposal{ID: "p1", Status: core.ProposalPending, RawText: "恢复的提案"},
		}}},
		Plan: &conversation.PlanState{Mode: sessionlog.PlanModePlan, PlanPath: ".stable/plans/s1.md"},
	}}})
	m = updated.(Model)
	if !m.planModeActive() {
		t.Fatal("restored plan state not picked up")
	}
	if view := m.View(); !strings.Contains(view, "计划模式") {
		t.Fatalf("plan mode segment missing after restore:\n%s", view)
	}
	// 恢复的提案与提问不自动弹层。
	if got := m.pendingDialog(); got != DialogNone {
		t.Fatalf("restore popped a dialog: %v", got)
	}
	updated, _ = m.handleResult(resultMsg{op: "question_list", msgs: []conversation.ServerMsg{{Type: "questions", Questions: []sessionlog.PendingQuestion{{QuestionID: "q1", SessionID: "s1", Status: sessionlog.QuestionPending, Prompt: "继续吗？"}}}}})
	m = updated.(Model)
	if _, ok := m.popupQuestion(); ok {
		t.Fatal("restored question must not be live/popup-eligible")
	}
	if got := m.pendingDialog(); got != DialogNone {
		t.Fatalf("restored question popped a dialog: %v", got)
	}
	// 计划审批属于运行中阻塞：恢复后有 pending 就弹。
	m.applyRunMessage(conversation.ServerMsg{Type: "plan_approval_pending", PlanApprovals: []conversation.PlanApprovalRef{{ID: "pa1", SessionID: "s1", PlanPath: ".stable/plans/s1.md"}}})
	if got := m.pendingDialog(); got != DialogPlan {
		t.Fatalf("restored pending plan approval must pop, got %v", got)
	}
	// plan_state 推送刷新状态栏；退回默认模式后段消失（先解决审批让聊天视图露出）。
	m.applyRunMessage(conversation.ServerMsg{Type: "plan_state", PlanState: &conversation.PlanState{Mode: sessionlog.PlanModeDefault}})
	m.clearPlanApprovals()
	if m.planModeActive() || strings.Contains(m.View(), "计划模式") {
		t.Fatal("plan segment must disappear after returning to default mode")
	}
}
