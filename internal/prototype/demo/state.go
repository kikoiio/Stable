package demo

import "fmt"

type Panel string

const (
	PanelSessions Panel = "sessions"
	PanelChat     Panel = "chat"
	PanelGoals    Panel = "goals"
)

type Message struct {
	Role   string
	Text   string
	Demo   bool
	GoalID string
}

type Criterion struct {
	Name        string
	Description string
}

type Proposal struct {
	ID              string
	Description     string
	Criteria        []Criterion
	Reason          string
	Status          string
	SourceSessionID string
	Demo            bool
}

type Session struct {
	ID       string
	Title    string
	Messages []Message
	Pending  *Proposal
}

type Steering struct {
	Text            string
	SourceSessionID string
	Demo            bool
}

type Goal struct {
	ID              string
	Description     string
	Status          string
	Reason          string
	Evidence        string
	SourceSessionID string
	Steering        []Steering
	Demo            bool
}

type State struct {
	Sessions        []Session
	Goals           []Goal
	ActiveSessionID string
	SelectedGoalID  string
	ActivePanel     Panel
	NextID          int
}

type ActionType string

const (
	CreateSession   ActionType = "create_session"
	SelectSession   ActionType = "select_session"
	SelectGoal      ActionType = "select_goal"
	SelectPanel     ActionType = "select_panel"
	SubmitGoal      ActionType = "submit_goal"
	ConfirmProposal ActionType = "confirm_proposal"
	RejectProposal  ActionType = "reject_proposal"
	SendSteering    ActionType = "send_steering"
)

type Action struct {
	Type ActionType
	ID   string
	Text string
}

// NewState returns a fresh, process-local copy of the fixed demo fixture.
func NewState() State {
	return State{
		Sessions: []Session{
			{
				ID: "session-electronics", Title: "传感器连接",
				Messages: []Message{
					{Role: "user", Text: "检查 RT1 和 J1 的连接关系", Demo: true},
					{Role: "assistant", Text: "这是固定演示会话，可审阅待确认提案。", Demo: true},
				},
				Pending: &Proposal{
					ID: "prop-demo-1", Description: "确认 RT1.2 与 J1.2 连通，并保持 ERC 清洁",
					Criteria: []Criterion{
						{Name: "sensor-j1-connection", Description: "RT1.2 与 J1.2 连通"},
						{Name: "erc-clean", Description: "ERC 违规数为 0"},
					},
					Reason: "固定 fixture 提案，仅用于演示审阅流程。",
					Status: "pending", SourceSessionID: "session-electronics", Demo: true,
				},
			},
			{
				ID: "session-notes", Title: "项目笔记",
				Messages: []Message{
					{Role: "user", Text: "整理一下本周的设计笔记", Demo: true},
					{Role: "assistant", Text: "这里展示另一个 session 的独立聊天历史。", Demo: true},
				},
			},
		},
		Goals: []Goal{
			{ID: "goal-demo-verified", Description: "整理连接器引脚映射", Status: "verified", Evidence: "固定证据摘要：检查报告通过。", SourceSessionID: "session-electronics", Demo: true},
			{ID: "goal-demo-waiting", Description: "检查外壳装配间距", Status: "needs_human", Reason: "等待人工确认测量基准。", Evidence: "固定证据摘要：尚无最终尺寸复核。", SourceSessionID: "session-notes", Demo: true},
		},
		ActiveSessionID: "session-electronics",
		SelectedGoalID:  "goal-demo-verified",
		ActivePanel:     PanelChat,
		NextID:          1,
	}
}

// Apply returns a copy of state with one deterministic demo transition applied.
func Apply(state State, action Action) State {
	s := clone(state)
	switch action.Type {
	case CreateSession:
		id := fmt.Sprintf("session-demo-%d", s.NextID)
		s.NextID++
		s.Sessions = append(s.Sessions, Session{ID: id, Title: fmt.Sprintf("新会话 %d", s.NextID-1)})
		s.ActiveSessionID = id
		s.ActivePanel = PanelChat
	case SelectSession:
		if findSession(&s, action.ID) != nil {
			s.ActiveSessionID = action.ID
		}
	case SelectGoal:
		if findGoal(&s, action.ID) != nil {
			s.SelectedGoalID = action.ID
			s.ActivePanel = PanelGoals
		}
	case SelectPanel:
		if action.ID == string(PanelSessions) || action.ID == string(PanelChat) || action.ID == string(PanelGoals) {
			s.ActivePanel = Panel(action.ID)
		}
	case SubmitGoal:
		text := trim(action.Text)
		if text == "" {
			break
		}
		session := findSession(&s, s.ActiveSessionID)
		if session == nil {
			break
		}
		session.Messages = append(session.Messages, Message{Role: "user", Text: text, Demo: true})
		session.Pending = &Proposal{
			ID: fmt.Sprintf("prop-demo-%d", s.NextID), Description: text,
			Criteria: []Criterion{
				{Name: "demo-goal-created", Description: "提案确认后在全局列表创建模拟目标"},
				{Name: "demo-visible", Description: "目标来源与 Demo 标记清晰可见"},
			},
			Reason: "固定演示模板根据输入展示提案流程；没有调用模型。",
			Status: "pending", SourceSessionID: s.ActiveSessionID, Demo: true,
		}
		s.NextID++
		s.ActivePanel = PanelChat
	case ConfirmProposal:
		session := findSession(&s, s.ActiveSessionID)
		if session == nil || session.Pending == nil || (action.ID != "" && session.Pending.ID != action.ID) {
			break
		}
		proposal := *session.Pending
		if proposal.Status != "pending" {
			break
		}
		proposal.Status = "confirmed"
		id := fmt.Sprintf("goal-demo-%d", s.NextID)
		s.NextID++
		s.Goals = append(s.Goals, Goal{
			ID: id, Description: proposal.Description, Status: "queued",
			Reason:          "Demo 目标已确认，实际执行未启动。",
			Evidence:        "暂无真实证据；这是固定原型数据。",
			SourceSessionID: proposal.SourceSessionID, Demo: true,
		})
		s.SelectedGoalID = id
		session.Pending = nil
		session.Messages = append(session.Messages, Message{Role: "system", Text: "Demo 提案已确认，已加入全局模拟目标。", Demo: true})
		s.ActivePanel = PanelGoals
	case RejectProposal:
		session := findSession(&s, s.ActiveSessionID)
		if session == nil || session.Pending == nil || (action.ID != "" && session.Pending.ID != action.ID) {
			break
		}
		session.Pending = nil
		session.Messages = append(session.Messages, Message{Role: "system", Text: "Demo 提案已拒绝；没有创建目标。", Demo: true})
	case SendSteering:
		text := trim(action.Text)
		if text == "" {
			break
		}
		goal := findGoal(&s, s.SelectedGoalID)
		if goal == nil {
			break
		}
		goal.Steering = append(goal.Steering, Steering{Text: text, SourceSessionID: s.ActiveSessionID, Demo: true})
		session := findSession(&s, s.ActiveSessionID)
		if session != nil {
			session.Messages = append(session.Messages, Message{Role: "user", Text: text, Demo: true, GoalID: goal.ID})
		}
		s.ActivePanel = PanelGoals
	}
	return s
}

func clone(in State) State {
	out := in
	out.Sessions = make([]Session, len(in.Sessions))
	for i, session := range in.Sessions {
		out.Sessions[i] = session
		out.Sessions[i].Messages = append([]Message(nil), session.Messages...)
		if session.Pending != nil {
			proposal := *session.Pending
			proposal.Criteria = append([]Criterion(nil), session.Pending.Criteria...)
			out.Sessions[i].Pending = &proposal
		}
	}
	out.Goals = make([]Goal, len(in.Goals))
	for i, goal := range in.Goals {
		out.Goals[i] = goal
		out.Goals[i].Steering = append([]Steering(nil), goal.Steering...)
	}
	return out
}

func findSession(s *State, id string) *Session {
	for i := range s.Sessions {
		if s.Sessions[i].ID == id {
			return &s.Sessions[i]
		}
	}
	return nil
}

func findGoal(s *State, id string) *Goal {
	for i := range s.Goals {
		if s.Goals[i].ID == id {
			return &s.Goals[i]
		}
	}
	return nil
}

func trim(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
