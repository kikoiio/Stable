package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
)

// planApprovalOptions is the fixed three-choice set of the plan approval
// dialog; the cursor selects one and Enter confirms it.
var planApprovalOptions = [3]struct{ title, detail string }{
	{"自动接受", "后续运行范围内写自动允许（写仍进候选区）"},
	{"逐次确认", "后续运行保持逐项确认的权限模式"},
	{"提供反馈", "输入纠偏文本，保持计划模式继续修改"},
}

// renderPlanDialog shows the plan approval request: the plan path, the three
// choices under the cursor, and — while the third choice is selected — the
// feedback input line.
func renderPlanDialog(approval conversation.PlanApprovalRef, cursor int, feedback string, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "计划审批\n计划文件：%s\n", approval.PlanPath)
	if approval.RunID != "" {
		fmt.Fprintf(&b, "运行：%s\n", approval.RunID)
	}
	b.WriteString("\n")
	for i, option := range planApprovalOptions {
		mark := "  "
		if i == clamp(cursor, 0, 2) {
			mark = "> "
		}
		fmt.Fprintf(&b, "%s%d %s：%s\n", mark, i+1, option.title, option.detail)
	}
	if clamp(cursor, 0, 2) == 2 {
		if strings.TrimSpace(feedback) == "" {
			b.WriteString("\n反馈：<输入纠偏文本>\n")
		} else {
			fmt.Fprintf(&b, "\n反馈：%s▏\n", feedback)
		}
	}
	b.WriteString("\n↑/↓ 选择 · Enter 确认 · Esc 取消审批")
	return truncateReviewLines(b.String(), width)
}

// activePlanApproval returns the plan approval of the active session;
// plan_resolve answers the session's single pending request.
func (m Model) activePlanApproval() (conversation.PlanApprovalRef, bool) {
	for _, approval := range m.PlanApprovals {
		if m.ActiveSession != "" && approval.SessionID != m.ActiveSession {
			continue
		}
		return approval, true
	}
	return conversation.PlanApprovalRef{}, false
}

// handlePlanKey drives the plan approval dialog: the cursor moves between the
// three choices, Enter resolves the request (auto, manual, or feedback with
// the typed text), and Esc cancels the approval so the run continues from the
// rejection. While the feedback choice is selected, printable keys edit the
// feedback line instead of triggering choices.
func (m Model) handlePlanKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if _, ok := m.activePlanApproval(); !ok {
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		if m.SelectedPlan > 0 {
			m.SelectedPlan--
		}
		return m, nil
	case "down", "j":
		if m.SelectedPlan < 2 {
			m.SelectedPlan++
		}
		return m, nil
	case "enter":
		switch clamp(m.SelectedPlan, 0, 2) {
		case 0:
			return m.resolvePlan(conversation.PlanResolveAuto, "")
		case 1:
			return m.resolvePlan(conversation.PlanResolveManual, "")
		default:
			feedback := strings.TrimSpace(m.PlanFeedback)
			if feedback == "" {
				m.Status = "请输入反馈文本，或选择其他选项。"
				return m, nil
			}
			return m.resolvePlan(conversation.PlanResolveFeedback, feedback)
		}
	case "backspace":
		if m.SelectedPlan == 2 {
			if runes := []rune(m.PlanFeedback); len(runes) > 0 {
				m.PlanFeedback = string(runes[:len(runes)-1])
			}
			return m, nil
		}
	case "esc":
		return m.resolvePlan(conversation.PlanResolveCancel, "")
	}
	if m.SelectedPlan == 2 && (k.Type == tea.KeyRunes || k.Type == tea.KeySpace) {
		m.PlanFeedback += k.String()
		return m, nil
	}
	return m, nil
}

// planResolveRequest builds the plan_resolve client message; plan choices are
// session-scoped, so the request carries no approval ID.
func planResolveRequest(sessionID, choice, feedback string) conversation.ClientMsg {
	return conversation.ClientMsg{Op: "plan_resolve", SessionID: sessionID, ApprovalChoice: choice, Text: feedback}
}

// resolvePlan sends the plan_resolve op for the session's pending request.
// The dialog stays up until the service reports the resolution and clears it.
func (m Model) resolvePlan(choice, feedback string) (tea.Model, tea.Cmd) {
	m.Pending = true
	m.Status = "正在提交计划审批决定…"
	return m, requestCmd(m.Socket, planResolveRequest(m.ActiveSession, choice, feedback))
}
