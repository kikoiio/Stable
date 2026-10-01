package prototype

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/prototype/demo"
)

type inputMode string

const (
	inputNone     inputMode = ""
	inputGoal     inputMode = "goal"
	inputSteering inputMode = "steering"
)

type Model struct {
	State         demo.State
	Width         int
	Height        int
	SessionCursor int
	GoalCursor    int
	mode          inputMode
	input         textinput.Model
	status        string
}

func NewModel() Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "输入文本后回车；Esc 取消"
	return Model{State: demo.NewState(), input: input, Width: 120, Height: 32}
}

func (m Model) Init() tea.Cmd { return textinput.Blink }

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.mode != inputNone {
			return m.updateInput(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = inputNone
		m.input.SetValue("")
		m.input.Blur()
		m.status = "已取消输入。"
		return m, nil
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		if value == "" {
			m.status = "请输入非空文本。"
			return m, nil
		}
		if m.mode == inputGoal {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.SubmitGoal, Text: value})
			m.status = "已生成固定 Demo 提案；没有调用模型。"
		} else {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.SendSteering, Text: value})
			m.status = "纠偏已记入选中目标的 Demo 记录。"
		}
		m.mode = inputNone
		m.input.SetValue("")
		m.input.Blur()
		m.syncCursors()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q":
		return m, tea.Quit
	case "tab":
		switch m.State.ActivePanel {
		case demo.PanelSessions:
			m.selectPanel(demo.PanelChat)
		case demo.PanelChat:
			m.selectPanel(demo.PanelGoals)
		default:
			m.selectPanel(demo.PanelSessions)
		}
	case "1":
		m.selectPanel(demo.PanelSessions)
	case "2":
		m.selectPanel(demo.PanelChat)
	case "3":
		m.selectPanel(demo.PanelGoals)
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "enter":
		m.activateCursor()
	case "s":
		m.State = demo.Apply(m.State, demo.Action{Type: demo.CreateSession})
		m.SessionCursor = len(m.State.Sessions) - 1
		m.status = "已创建仅驻留内存的 Demo session。"
	case "n":
		m.mode = inputGoal
		m.input.Placeholder = "描述一个目标（固定 Demo 提案）"
		m.input.Focus()
		return m, textinput.Blink
	case "r":
		if m.State.SelectedGoalID == "" {
			m.status = "请先选择一个目标。"
			return m, nil
		}
		m.mode = inputSteering
		m.input.Placeholder = "输入发给选中目标的 Demo 纠偏"
		m.input.Focus()
		return m, textinput.Blink
	case "y":
		if proposal := activeProposal(m.State); proposal != nil {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.ConfirmProposal, ID: proposal.ID})
			m.status = "Demo 提案已确认并加入全局目标。"
		} else {
			m.status = "当前 session 没有待审提案。"
		}
	case "x":
		if proposal := activeProposal(m.State); proposal != nil {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.RejectProposal, ID: proposal.ID})
			m.status = "Demo 提案已拒绝；没有创建目标。"
		} else {
			m.status = "当前 session 没有待审提案。"
		}
	case "esc":
		m.status = "使用 q 退出原型。"
	}
	m.syncCursors()
	return m, nil
}

func (m *Model) selectPanel(panel demo.Panel) {
	m.State = demo.Apply(m.State, demo.Action{Type: demo.SelectPanel, ID: string(panel)})
}

func (m *Model) moveCursor(delta int) {
	switch m.State.ActivePanel {
	case demo.PanelSessions:
		if len(m.State.Sessions) == 0 {
			return
		}
		m.SessionCursor = clamp(m.SessionCursor+delta, 0, len(m.State.Sessions)-1)
		m.State = demo.Apply(m.State, demo.Action{Type: demo.SelectSession, ID: m.State.Sessions[m.SessionCursor].ID})
	case demo.PanelGoals:
		if len(m.State.Goals) == 0 {
			return
		}
		m.GoalCursor = clamp(m.GoalCursor+delta, 0, len(m.State.Goals)-1)
		m.State = demo.Apply(m.State, demo.Action{Type: demo.SelectGoal, ID: m.State.Goals[m.GoalCursor].ID})
	}
}

func (m *Model) activateCursor() {
	switch m.State.ActivePanel {
	case demo.PanelSessions:
		if m.SessionCursor < len(m.State.Sessions) {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.SelectSession, ID: m.State.Sessions[m.SessionCursor].ID})
		}
	case demo.PanelGoals:
		if m.GoalCursor < len(m.State.Goals) {
			m.State = demo.Apply(m.State, demo.Action{Type: demo.SelectGoal, ID: m.State.Goals[m.GoalCursor].ID})
		}
	}
}

func (m *Model) syncCursors() {
	for i, session := range m.State.Sessions {
		if session.ID == m.State.ActiveSessionID {
			m.SessionCursor = i
			break
		}
	}
	for i, goal := range m.State.Goals {
		if goal.ID == m.State.SelectedGoalID {
			m.GoalCursor = i
			break
		}
	}
}

func activeProposal(state demo.State) *demo.Proposal {
	for i := range state.Sessions {
		if state.Sessions[i].ID == state.ActiveSessionID {
			return state.Sessions[i].Pending
		}
	}
	return nil
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (m Model) View() string { return render(m) }
