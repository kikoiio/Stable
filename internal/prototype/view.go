package prototype

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"stable/internal/prototype/demo"
)

var (
	borderStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63"))
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	demoStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)

func render(m Model) string {
	if m.Width < 40 || m.Height < 10 {
		return demoStyle.Render("STABLE PROTOTYPE / DEMO") + "\n终端空间过小，请扩大窗口（至少 40×10）。\n按 q 退出。"
	}
	var body string
	if m.Width < 120 {
		body = narrowView(m)
	} else {
		body = wideView(m)
	}
	footer := "1 Sessions  2 Chat  3 Goals  Tab 切换面板  ↑/↓ 移动  Enter 选择  s 新建 session  n 新目标  y 确认  x 拒绝  r 纠偏  q 退出"
	if m.mode != inputNone {
		prompt := "目标描述："
		if m.mode == inputSteering {
			prompt = "发送 Demo 纠偏："
		}
		footer = prompt + m.input.View() + "\nEnter 提交 · Esc 取消 · q 退出"
	}
	if m.status != "" {
		footer += "\n" + mutedStyle.Render(m.status)
	}
	return demoStyle.Render("STABLE PROTOTYPE / DEMO — 固定模拟数据 · 不连接生产运行时") + "\n" + body + "\n" + mutedStyle.Render(footer)
}

func wideView(m Model) string {
	available := m.Width - 8
	left := available * 22 / 100
	middle := available * 48 / 100
	right := available - left - middle
	height := max(m.Height-8, 8)
	panels := []string{
		panel("1 SESSIONS", renderSessions(m), left, height, m.State.ActivePanel == demo.PanelSessions),
		panel("2 CHAT / PROPOSAL", renderChat(m), middle, height, m.State.ActivePanel == demo.PanelChat),
		panel("3 GLOBAL GOALS", renderGoals(m), right, height, m.State.ActivePanel == demo.PanelGoals),
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, panels...)
}

func narrowView(m Model) string {
	width := m.Width - 4
	height := max(m.Height-8, 8)
	var title, content string
	switch m.State.ActivePanel {
	case demo.PanelSessions:
		title, content = "1 SESSIONS", renderSessions(m)
	case demo.PanelGoals:
		title, content = "3 GLOBAL GOALS", renderGoals(m)
	default:
		title, content = "2 CHAT / PROPOSAL", renderChat(m)
	}
	return panel(title, content, width, height, true)
}

func panel(title, content string, width, height int, focused bool) string {
	style := borderStyle.Width(width).Height(height)
	if focused {
		style = style.BorderForeground(lipgloss.Color("205"))
	}
	return style.Render(selectedStyle.Render(title) + "\n" + content)
}

func renderSessions(m Model) string {
	var b strings.Builder
	for i, session := range m.State.Sessions {
		prefix := "  "
		if session.ID == m.State.ActiveSessionID {
			prefix = "› "
		}
		if i == m.SessionCursor {
			b.WriteString(selectedStyle.Render(prefix + session.Title))
		} else {
			b.WriteString(prefix + session.Title)
		}
		if session.Pending != nil {
			b.WriteString("  [待审 Demo]")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n[s] 新建会话")
	return b.String()
}

func renderChat(m Model) string {
	var b strings.Builder
	session := findActive(m.State)
	if session == nil {
		return "没有活动 session。"
	}
	fmt.Fprintf(&b, "Session：%s\n\n", session.Title)
	for _, message := range session.Messages {
		role := message.Role
		if message.Demo {
			role += " · DEMO"
		}
		fmt.Fprintf(&b, "%s：%s\n\n", role, message.Text)
	}
	if session.Pending != nil {
		p := session.Pending
		fmt.Fprintf(&b, "%s 提案 %s\n目标：%s\n理由：%s\n验收标准：\n", demoStyle.Render("待审 DEMO"), p.ID, p.Description, p.Reason)
		for _, criterion := range p.Criteria {
			fmt.Fprintf(&b, " • %s — %s\n", criterion.Name, criterion.Description)
		}
		b.WriteString("\ny 确认 · x 拒绝\n")
	}
	return b.String()
}

func renderGoals(m Model) string {
	var b strings.Builder
	for i, goal := range m.State.Goals {
		prefix := "  "
		if goal.ID == m.State.SelectedGoalID {
			prefix = "› "
		}
		line := fmt.Sprintf("%s%s [%s]", prefix, goal.Description, goal.Status)
		if i == m.GoalCursor {
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString(line)
		}
		fmt.Fprintf(&b, "\n  %s · 来源 %s\n", demoStyle.Render("DEMO"), goal.SourceSessionID)
		if goal.ID == m.State.SelectedGoalID {
			if goal.Reason != "" {
				fmt.Fprintf(&b, "  原因：%s\n", goal.Reason)
			}
			if goal.Evidence != "" {
				fmt.Fprintf(&b, "  证据：%s\n", goal.Evidence)
			}
			for _, steering := range goal.Steering {
				fmt.Fprintf(&b, "  纠偏 DEMO：%s（%s）\n", steering.Text, steering.SourceSessionID)
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n[n] 提案  [r] 纠偏")
	return b.String()
}

func findActive(state demo.State) *demo.Session {
	for i := range state.Sessions {
		if state.Sessions[i].ID == state.ActiveSessionID {
			return &state.Sessions[i]
		}
	}
	return nil
}
