package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
)

func renderRemoteAccessDialog(requests []conversation.RemoteAccessRequest, selected, width int) string {
	if len(requests) == 0 {
		return "没有待批准的远程目录请求。"
	}
	request := requests[clamp(selected, 0, len(requests)-1)]
	var b strings.Builder
	fmt.Fprintf(&b, "远程客户端请求访问目录 · %d/%d\n客户端：%s\n项目目录：%s\n请求到期：%s\n\n",
		selected+1, len(requests), safeDialogText(request.ClientLabel), safeDialogText(request.ProjectRoot), request.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	b.WriteString("1 批准当前连接 · 2 拒绝 · Esc 拒绝 · ↑/↓ 切换请求")
	return truncateReviewLines(b.String(), width)
}

func (m Model) handleRemoteAccessKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.RemoteAccessRequests) == 0 {
		return m, nil
	}
	selected := clamp(m.SelectedRemoteAccess, 0, len(m.RemoteAccessRequests)-1)
	request := m.RemoteAccessRequests[selected]
	decision := ""
	switch key.String() {
	case "up", "k":
		if m.SelectedRemoteAccess > 0 {
			m.SelectedRemoteAccess--
		}
		return m, nil
	case "down", "j":
		if m.SelectedRemoteAccess < len(m.RemoteAccessRequests)-1 {
			m.SelectedRemoteAccess++
		}
		return m, nil
	case "1":
		decision = "approve"
	case "2", "esc":
		decision = "deny"
	default:
		return m, nil
	}
	m.Pending = true
	if decision == "approve" {
		m.Status = "正在批准远程目录访问…"
	} else {
		m.Status = "正在拒绝远程目录访问…"
	}
	m.RemoteAccessRequests = append(m.RemoteAccessRequests[:selected], m.RemoteAccessRequests[selected+1:]...)
	if m.SelectedRemoteAccess >= len(m.RemoteAccessRequests) {
		m.SelectedRemoteAccess = max(0, len(m.RemoteAccessRequests)-1)
	}
	return m, requestCmd(m.Socket, conversation.ClientMsg{
		Op: "remote_access_resolve", RemoteAccessRequestID: request.ID, RemoteAccessDecision: decision,
	})
}
