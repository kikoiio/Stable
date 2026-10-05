package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
	"stable/internal/core"
)

// renderProposalDialog shows one goal proposal: the raw objective text and
// the full acceptance criteria, with the confirm / reject / defer keys.
func renderProposalDialog(proposal core.CriteriaProposal, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标提案 %s\n\n目标描述\n%s\n\n验收标准\n", proposal.ID, sanitizeReviewText(proposal.RawText))
	if len(proposal.Criteria) == 0 {
		b.WriteString("（无）\n")
	}
	for _, criterion := range proposal.Criteria {
		payload := strings.TrimSpace(string(criterion.Payload))
		if payload == "" || payload == "null" {
			fmt.Fprintf(&b, "- %s\n", criterion.Kind)
		} else {
			fmt.Fprintf(&b, "- %s %s\n", criterion.Kind, payload)
		}
	}
	b.WriteString("\nc 确认提案 · r 拒绝提案 · Esc 稍后（提案保持待定，/confirm 与 /reject 仍可用）")
	return truncateReviewLines(b.String(), width)
}

// popupProposal returns the first live, undecided proposal the user has not
// deferred. Proposals restored from the session transcript are never marked
// live, so a restored session never pops the dialog (spec F6); /confirm and
// /reject keep working for every pending proposal.
func (m Model) popupProposal() (core.CriteriaProposal, bool) {
	for _, p := range m.Proposals {
		if !m.liveProposals[p.ID] || m.proposalDismissed[p.ID] {
			continue
		}
		if p.Status != "" && p.Status != core.ProposalPending {
			continue
		}
		return p, true
	}
	return core.CriteriaProposal{}, false
}

// handleProposalKey drives the proposal dialog: c confirms and r rejects
// through the existing confirm/reject ops, Esc defers the dialog without
// touching the proposal state.
func (m Model) handleProposalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	proposal, ok := m.popupProposal()
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "c":
		return m.sendProposalDecision(proposal, "confirm", "正在记录提案确认…")
	case "r":
		return m.sendProposalDecision(proposal, "reject", "正在记录提案拒绝…")
	case "esc":
		m.deferProposal(proposal.ID)
		m.Status = "提案已稍后：仍可用 /confirm 或 /reject 处理。"
		return m, nil
	}
	return m, nil
}

// sendProposalDecision submits the confirm/reject op for one proposal. The
// dialog defers the proposal immediately so a failed op never re-pops it; the
// text commands stay available either way.
func (m Model) sendProposalDecision(proposal core.CriteriaProposal, op, status string) (tea.Model, tea.Cmd) {
	m.deferProposal(proposal.ID)
	m.Pending = true
	m.Status = status
	return m, requestCmd(m.Socket, conversation.ClientMsg{Op: op, ID: proposal.ID, SessionID: m.ActiveSession})
}

func (m *Model) deferProposal(id string) {
	if m.proposalDismissed == nil {
		m.proposalDismissed = map[string]bool{}
	}
	m.proposalDismissed[id] = true
}
