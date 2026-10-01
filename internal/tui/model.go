package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/sessionlog"
)

type Panel int

const (
	SessionsPanel Panel = iota
	ChatPanel
	GoalsPanel
)

type InputMode string

const (
	ChatInput  InputMode = "chat"
	GoalInput  InputMode = "goal"
	SayInput   InputMode = "say"
	ReplyInput InputMode = "reply"
)

type Model struct {
	Socket, Root     string
	Width, Height    int
	Panel            Panel
	Sessions         []sessionlog.SessionInfo
	ActiveSession    string
	Events           []sessionlog.Event
	Goals            []core.Goal
	SelectedGoal     int
	Proposals        []core.CriteriaProposal
	SelectedProposal int
	Input            textinput.Model
	Mode             InputMode
	Pending          bool
	Status           string
	Err              error
}

type resultMsg struct {
	op   string
	msgs []conversation.ServerMsg
	err  error
}

func New(socket, root string) Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "输入消息后回车"
	return Model{Socket: socket, Root: root, Width: 120, Height: 32, Input: in}
}

func (m Model) Init() tea.Cmd {
	return requestCmd(m.Socket, conversation.ClientMsg{Op: "session_list", ProjectRoot: m.Root})
}
func requestCmd(socket string, req conversation.ClientMsg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		msgs, err := conversation.Request(ctx, socket, req)
		return resultMsg{op: req.Op, msgs: msgs, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = v.Width, v.Height
		return m, nil
	case resultMsg:
		return m.handleResult(v)
	case tea.KeyMsg:
		if v.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.Mode != "" {
			return m.handleInput(v)
		}
		return m.handleKey(v)
	}
	return m, nil
}

func (m Model) handleResult(r resultMsg) (tea.Model, tea.Cmd) {
	m.Pending = false
	if r.err != nil {
		m.Err = r.err
		m.Status = "请求失败；已写入的用户消息仍保存在当前会话：" + r.err.Error()
		if (r.op == "chat" || r.op == "create_goal") && m.ActiveSession != "" {
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
		return m, nil
	}
	if !strings.HasPrefix(m.Status, "请求失败") || r.op != "session_load" {
		m.Err = nil
	}
	for _, x := range r.msgs {
		switch x.Type {
		case "sessions":
			m.Sessions = x.Sessions
			m.Goals = x.Goals
			if len(m.Sessions) == 0 {
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: m.Root})
			}
			m.ActiveSession = m.Sessions[0].ID
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		case "session":
			if x.Session != nil {
				m.Sessions = append([]sessionlog.SessionInfo{*x.Session}, m.Sessions...)
				m.ActiveSession = x.Session.ID
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
			}
		case "transcript":
			if x.Transcript != nil {
				m.Events = x.Transcript.Events
			}
			if x.Goals != nil {
				m.Goals = x.Goals
			}
			m.Proposals = proposalsFromEvents(m.Events)
			if m.Err == nil {
				m.Status = "会话已加载。"
			}
		case "goal_update":
			if x.Goal != nil {
				m.upsertGoal(*x.Goal)
			}
		case "proposal":
			if x.Proposal != nil {
				m.upsertProposal(*x.Proposal)
			}
		}
	}
	if r.op == "chat" || r.op == "create_goal" || r.op == "confirm" || r.op == "reject" {
		if m.ActiveSession != "" {
			m.Pending = true
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
	}
	return m, nil
}

func proposalsFromEvents(events []sessionlog.Event) []core.CriteriaProposal {
	var out []core.CriteriaProposal
	for _, e := range events {
		if e.Type != sessionlog.EventProposal {
			continue
		}
		b, _ := json.Marshal(e.Data)
		var p core.CriteriaProposal
		if json.Unmarshal(b, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}
func (m *Model) upsertGoal(g core.Goal) {
	for i := range m.Goals {
		if m.Goals[i].ID == g.ID {
			m.Goals[i] = g
			return
		}
	}
	m.Goals = append(m.Goals, g)
}
func (m *Model) upsertProposal(p core.CriteriaProposal) {
	for i := range m.Proposals {
		if m.Proposals[i].ID == p.ID {
			m.Proposals[i] = p
			return
		}
	}
	m.Proposals = append(m.Proposals, p)
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "tab":
		m.Panel = (m.Panel + 1) % 3
	case "1":
		m.Panel = SessionsPanel
	case "2":
		m.Panel = ChatPanel
	case "3":
		m.Panel = GoalsPanel
	case "up", "k":
		if m.Panel == SessionsPanel && len(m.Sessions) > 0 {
			m.SelectedSession(-1)
		} else if m.Panel == GoalsPanel && m.SelectedGoal > 0 {
			m.SelectedGoal--
		} else if m.Panel == ChatPanel && m.SelectedProposal > 0 {
			m.SelectedProposal--
		}
	case "down", "j":
		if m.Panel == SessionsPanel && len(m.Sessions) > 1 {
			m.SelectedSession(1)
		} else if m.Panel == GoalsPanel && m.SelectedGoal+1 < len(m.Goals) {
			m.SelectedGoal++
		} else if m.Panel == ChatPanel && m.SelectedProposal+1 < len(m.Proposals) {
			m.SelectedProposal++
		}
	case "enter":
		if m.Panel == SessionsPanel && len(m.Sessions) > 0 {
			m.ActiveSession = m.Sessions[clamp(m.sessionIndex(), 0, len(m.Sessions)-1)].ID
			m.Pending = true
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		} else {
			m.Mode = ChatInput
			m.Input.Placeholder = "输入普通聊天消息"
			m.Input.Focus()
			return m, textinput.Blink
		}
	case "s":
		m.Pending = true
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: m.Root})
	case "n":
		m.startInput(GoalInput, "描述一个新目标；提交后会生成待审验收标准")
	case "r":
		if len(m.Goals) == 0 {
			m.Status = "请先选择一个全局目标。"
		} else {
			m.startInput(SayInput, "输入发给选中目标的纠偏")
		}
	case "p":
		if len(m.Goals) == 0 {
			m.Status = "请先选择一个全局目标。"
		} else {
			m.startInput(ReplyInput, "回复选中目标的待答问题")
		}
	case "y":
		if len(m.Proposals) > 0 {
			p := m.Proposals[clamp(m.SelectedProposal, 0, len(m.Proposals)-1)]
			if p.Status == core.ProposalPending {
				m.Pending = true
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "confirm", ID: p.ID, ProjectRoot: m.Root, SessionID: m.ActiveSession})
			}
		}
	case "x":
		if len(m.Proposals) > 0 {
			p := m.Proposals[clamp(m.SelectedProposal, 0, len(m.Proposals)-1)]
			if p.Status == core.ProposalPending {
				m.Pending = true
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "reject", ID: p.ID, ProjectRoot: m.Root, SessionID: m.ActiveSession})
			}
		}
	case "esc":
		m.Status = ""
	}
	return m, nil
}

func (m *Model) SelectedSession(delta int) {
	i := clamp(m.sessionIndex()+delta, 0, len(m.Sessions)-1)
	m.ActiveSession = m.Sessions[i].ID
}
func (m Model) sessionIndex() int {
	for i, s := range m.Sessions {
		if s.ID == m.ActiveSession {
			return i
		}
	}
	return 0
}
func (m *Model) startInput(mode InputMode, placeholder string) {
	m.Mode = mode
	m.Input.Placeholder = placeholder
	m.Input.SetValue("")
	m.Input.Focus()
}

func (m Model) handleInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.Mode = ""
		m.Input.Blur()
		m.Status = "已取消输入。"
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.Input.Value())
		if text == "" {
			m.Status = "请输入非空内容。"
			return m, nil
		}
		req := conversation.ClientMsg{ProjectRoot: m.Root, SessionID: m.ActiveSession, Text: text}
		if len(m.Goals) > 0 && m.Panel == GoalsPanel {
			req.Goal = m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID
		}
		switch m.Mode {
		case GoalInput:
			req.Op = "create_goal"
		case SayInput:
			req.Op = "say"
		case ReplyInput:
			req.Op = "reply"
			if len(m.Goals) > 0 {
				req.Goal = m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID
			}
		default:
			req.Op = "chat"
		}
		if req.Op == "say" {
			if len(m.Goals) > 0 {
				req.Goal = m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID
			}
		}
		m.Pending = true
		m.Mode = ""
		m.Input.SetValue("")
		m.Input.Blur()
		return m, requestCmd(m.Socket, req)
	}
	var cmd tea.Cmd
	m.Input, cmd = m.Input.Update(k)
	return m, cmd
}

func (m Model) View() string {
	if m.Width < 45 || m.Height < 12 {
		return "STABLE · production TUI\n终端窗口较小，请放大窗口。按 q 退出。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Stable · %s\n", m.ActiveSession)
	fmt.Fprintf(&b, "[1] Sessions (%d)   [2] Chat   [3] Global Goals\n\n", len(m.Sessions))
	if m.Width >= 112 {
		available := m.Width - 8
		left := available * 22 / 100
		middle := available * 48 / 100
		right := available - left - middle
		panelHeight := max(5, m.Height-13)
		panels := []string{renderPanel("SESSIONS", limited(m.sessionView(max(8, left-4)), panelHeight, false), left, panelHeight, m.Panel == SessionsPanel), renderPanel("CHAT / PROPOSALS", limited(m.chatView(max(8, middle-4)), panelHeight, true), middle, panelHeight, m.Panel == ChatPanel), renderPanel("GLOBAL GOALS", limited(m.goalView(max(8, right-4)), panelHeight, false), right, panelHeight, m.Panel == GoalsPanel)}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, panels...))
	} else {
		switch m.Panel {
		case SessionsPanel:
			fmt.Fprintf(&b, "%s\n", renderPanel("SESSIONS", limited(m.sessionView(m.Width-6), m.Height-14, false), m.Width-4, m.Height-13, true))
		case GoalsPanel:
			fmt.Fprintf(&b, "%s\n", renderPanel("GLOBAL GOALS", limited(m.goalView(m.Width-6), m.Height-14, false), m.Width-4, m.Height-13, true))
		default:
			fmt.Fprintf(&b, "%s\n", renderPanel("CHAT / PROPOSALS", limited(m.chatView(m.Width-6), m.Height-14, true), m.Width-4, m.Height-13, true))
		}
	}
	if m.Mode != "" {
		fmt.Fprintf(&b, "\n%s\n", m.Input.View())
	}
	if m.Pending {
		b.WriteString("\n等待服务响应… 导航和 q 退出仍可用。")
	}
	if m.Status != "" {
		fmt.Fprintf(&b, "\n%s", m.Status)
	}
	if m.Err != nil {
		fmt.Fprintf(&b, "\n错误：%s", m.Err.Error())
	}
	b.WriteString("\n\n↑/↓ 选择 · Enter 激活/聊天 · s 新建会话 · n 新目标 · y 确认 · x 拒绝 · r 纠偏 · p 回复 · q 退出")
	return b.String()
}

func renderPanel(title, content string, width, height int, focused bool) string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(max(4, width-2)).Height(max(3, height-3)).Padding(0, 1).BorderForeground(lipgloss.Color("240"))
	if focused {
		style = style.BorderForeground(lipgloss.Color("205"))
	}
	return style.Render(title + "\n" + content)
}
func limited(text string, maxLines int, keepLast bool) string {
	if maxLines < 1 {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines {
		return text
	}
	if keepLast {
		lines = lines[len(lines)-maxLines:]
	} else {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

func (m Model) sessionView(width int) string {
	var b strings.Builder
	for i, s := range m.Sessions {
		mark := " "
		if s.ID == m.ActiveSession {
			mark = "›"
		}
		title := s.Title
		if len(title) > width-4 {
			title = title[:max(1, width-4)]
		}
		fmt.Fprintf(&b, "%s %s\n", mark, title)
		if i >= 12 {
			break
		}
	}
	b.WriteString("\ns 新建")
	return b.String()
}
func (m Model) chatView(width int) string {
	var b strings.Builder
	shown := 0
	for _, e := range m.Events {
		if e.Type == sessionlog.EventMessage {
			var msg sessionlog.Message
			raw, _ := json.Marshal(e.Data)
			_ = json.Unmarshal(raw, &msg)
			label := msg.Role
			if label == "assistant" {
				label = "Stable"
			}
			text := strings.ReplaceAll(msg.Text, "\n", " ")
			if len(text) > width-12 {
				text = text[:max(1, width-12)] + "…"
			}
			fmt.Fprintf(&b, "%s: %s\n", label, text)
			shown++
		}
	}
	if shown == 0 {
		b.WriteString("空会话。按 Enter 开始聊天。\n")
	}
	if len(m.Proposals) > 0 {
		b.WriteString("\n待审提案：\n")
		for i, p := range m.Proposals {
			mark := " "
			if i == m.SelectedProposal {
				mark = "›"
			}
			fmt.Fprintf(&b, "%s %s [%s] · %d 项标准\n", mark, p.ID, p.Status, len(p.Criteria))
			if i == m.SelectedProposal {
				for _, c := range p.Criteria {
					fmt.Fprintf(&b, "  - %s (%s): %s\n", c.ID, c.Kind, string(c.Payload))
				}
			}
		}
	}
	return b.String()
}
func (m Model) goalView(width int) string {
	var b strings.Builder
	for i, g := range m.Goals {
		mark := " "
		if i == m.SelectedGoal {
			mark = "›"
		}
		name := g.Objective
		if name == "" {
			name = g.ID
		}
		if len(name) > width-12 {
			name = name[:max(1, width-12)] + "…"
		}
		fmt.Fprintf(&b, "%s %s [%s]\n", mark, name, g.Status)
		if i == m.SelectedGoal {
			if g.Reason != "" {
				fmt.Fprintf(&b, " 原因：%s\n", g.Reason)
			}
			if g.SourceSessionID != "" {
				fmt.Fprintf(&b, " 来源：%s\n", g.SourceSessionID)
			}
			if g.EvidenceSummary != "" {
				fmt.Fprintf(&b, " 证据：%s\n", g.EvidenceSummary)
			}
		}
	}
	if len(m.Goals) == 0 {
		b.WriteString("暂无目标。\n")
	}
	return b.String()
}
func clamp(n, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func Run(socket, root string) error {
	_, err := tea.NewProgram(New(socket, root), tea.WithAltScreen()).Run()
	return err
}
