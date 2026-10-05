package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/candidate"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/inputhistory"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/skills"
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
	Socket, Root        string
	Width, Height       int
	Panel               Panel // retained as an internal compatibility cursor for pre-M01 tests
	Sessions            []sessionlog.SessionInfo
	ActiveSession       string
	Events              []sessionlog.Event
	Goals               []core.Goal
	SelectedGoal        int
	Proposals           []core.CriteriaProposal
	SelectedProposal    int
	Composer            Composer
	Mode                InputMode
	Navigation          NavigationState
	Transcript          Transcript
	Layout              LayoutMetrics
	Candidates          []CompletionItem
	CandidateIndex      int
	Pending             bool
	Status              string
	Review              *candidate.Review
	ReviewCandidate     string
	ReviewConfirmed     map[string]bool
	ReviewCursor        int
	ReviewSnapshots     []sessionlog.SnapshotRef
	RewindPick          bool
	RewindCursor        int
	RewindArmed         bool
	Questions           []sessionlog.PendingQuestion
	SearchHits          []sessionlog.SearchHit
	SearchCorrupt       []sessionlog.SearchError
	Approvals           []permission.ApprovalPrompt
	SelectedApproval    int
	approvalPollStarted bool
	// Plan is the session plan-mode runtime state restored by session_load
	// and refreshed by plan_state pushes; it only feeds the status line.
	Plan              *conversation.PlanState
	PlanApprovals     []conversation.PlanApprovalRef
	SelectedPlan      int
	PlanFeedback      string
	Todos             []sessionlog.TaskSnapshot
	QuestionCursor    int
	QuestionPicked    map[int]bool
	QuestionOther     bool
	questionDialogID  string
	questionOtherText string
	questionDismissed map[string]bool
	liveQuestions     map[string]bool
	proposalDismissed map[string]bool
	liveProposals     map[string]bool
	Err               error
	ActiveRunID       string
	LastCursor        uint64
	stream            *conversation.StreamClient
	history           *inputhistory.Store
	historyErr        error
	histCursor        *inputhistory.Cursor
	histDraft         string
	registry          *commands.Registry
	loader            *commands.Loader
	host              *commandHost
	// skills is the TUI-side skill catalog: it feeds the /skills listing and
	// registers one slash command per skill behind the built-in and custom
	// command names. The conversation service keeps its own instance.
	skills *skills.Catalog
	// commandReportShown marks that the loader's rejected-file report has
	// already been surfaced once in the status line.
	commandReportShown bool
	// skillConflictShown marks that the skipped-skill-command report has
	// already been surfaced once in the status line.
	skillConflictShown bool
}

// commandHost bridges built-in Local closures with the live model: a
// commands.Command.Local only receives the raw arguments, so dispatchCommand
// points host at the model copy it is about to return, the closures mutate it
// through host.model, queue their tea.Cmds through send, and read host.raw
// when they need the exact submitted line for the input history.
type commandHost struct {
	model *Model
	raw   string
	cmds  []tea.Cmd
}

func (h *commandHost) send(cmd tea.Cmd) { h.cmds = append(h.cmds, cmd) }

type resultMsg struct {
	op   string
	msgs []conversation.ServerMsg
	err  error
}

type runStreamStartedMsg struct {
	client *conversation.StreamClient
	err    error
	resume bool
}
type runStreamMsg struct {
	client  *conversation.StreamClient
	message conversation.ServerMsg
	err     error
}
type approvalPollTick time.Time

func approvalPollCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(now time.Time) tea.Msg { return approvalPollTick(now) })
}

func openRunCmd(socket string, request agent.ExecutionRequest) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := conversation.OpenRun(ctx, socket, request)
		return runStreamStartedMsg{client: client, err: err}
	}
}
func receiveRunCmd(client *conversation.StreamClient) tea.Cmd {
	return func() tea.Msg {
		message, err := client.Receive()
		return runStreamMsg{client: client, message: message, err: err}
	}
}
func resumeRunCmd(socket, sessionID, runID string, cursor uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := conversation.SubscribeRun(ctx, socket, sessionID, runID, cursor)
		return runStreamStartedMsg{client: client, err: err, resume: true}
	}
}
func cancelRunCmd(client *conversation.StreamClient, sessionID, runID string) tea.Cmd {
	return func() tea.Msg {
		return runStreamMsg{client: client, message: conversation.ServerMsg{Type: "cancel_sent"}, err: client.Cancel(sessionID, runID)}
	}
}
func resubscribeRunCmd(client *conversation.StreamClient, sessionID, runID string, cursor uint64) tea.Cmd {
	return func() tea.Msg {
		return runStreamMsg{client: client, message: conversation.ServerMsg{Type: "resubscribed"}, err: client.Send(conversation.ClientMsg{Op: "run_subscribe", SessionID: sessionID, RunID: runID, AfterSeq: cursor})}
	}
}

func New(socket, root string) Model {
	c := NewComposer()
	_ = c.Focus()
	m := Model{Socket: socket, Root: root, Width: 120, Height: 32, Composer: c, Navigation: NavigationState{Mode: ChatView}}
	m.initCommands(root)
	// Input history is per-project and private; an unavailable store must not
	// block the TUI, only surface an explicit status when used.
	history, err := inputhistory.Open(root)
	if err != nil {
		m.historyErr = err
	} else {
		m.history = history
	}
	return m
}

// initCommands builds the command host, the initial built-in registry, and
// the custom command loader. Custom commands are loaded lazily by
// refreshCommands on the first completion or dispatch, never at startup.
func (m *Model) initCommands(root string) {
	m.host = &commandHost{}
	m.registry = commands.NewRegistry()
	registerBuiltins(m.host, m.registry)
	m.loader = commands.NewLoader(
		filepath.Join(root, ".stable", "commands"),
		userCommandsDir(),
	)
	m.skills = skills.LoadCatalog(userSkillsDir(), filepath.Join(root, ".stable", "skills"))
}

// userSkillsDir returns the user-level skill directory; an unavailable home
// directory or config base simply contributes no user skills.
func userSkillsDir() string {
	dir, err := appconfig.UserSkillsDir()
	if err != nil {
		return ""
	}
	return dir
}

// userCommandsDir returns the user-level custom command directory; an
// unavailable home directory simply contributes no user commands.
func userCommandsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "stable", "commands")
}

// refreshCommands rebuilds the registry from the fixed built-in set, the
// loader's custom commands, and one command per skill — in that order, so a
// custom file shadows a skill name and a built-in shadows both. Rebuilding
// instead of accumulating also lets a deleted custom file or skill disappear
// from completion and dispatch.
func (m *Model) refreshCommands() {
	if m.host == nil || m.loader == nil || m.registry == nil {
		m.initCommands(m.Root)
		return
	}
	registry := commands.NewRegistry()
	registerBuiltins(m.host, registry)
	if custom, rejected, err := m.loader.Commands(); err == nil {
		for _, c := range custom {
			registry.RegisterOptional(c)
		}
		m.surfaceCommandReport(rejected)
	}
	var conflicts []string
	if m.skills != nil {
		if m.skills.NeedsReload() {
			m.skills.Reload()
		}
		for _, s := range m.skills.List() {
			name, description := s.Meta.Name, s.Meta.Description
			command := &commands.Command{
				Name:        name,
				Description: "（技能）" + description,
				Kind:        commands.KindLocal,
				Local: func(args string) {
					m2 := m.host.model
					req := conversation.ClientMsg{Op: "skill_invoke", SessionID: m2.ActiveSession, SkillName: name, SkillArgs: args}
					m2.Pending = true
					m2.Status = "正在激活技能 " + name + "…"
					m2.Composer.SetValue("")
					m2.recordHistory(m.host.raw)
					m.host.send(requestCmd(m2.Socket, req))
				},
			}
			if !registry.RegisterOptional(command) {
				conflicts = append(conflicts, name)
			}
		}
	}
	if len(conflicts) > 0 && !m.skillConflictShown {
		m.skillConflictShown = true
		if m.Status == "" {
			m.Status = "部分技能命令与既有命令同名，已跳过：" + strings.Join(conflicts, "、")
		}
	}
	m.registry = registry
}

// surfaceCommandReport shows the loader's rejected-file report once in the
// status line, the same channel every other one-off hint uses. Later loads
// with the same report stay quiet.
func (m *Model) surfaceCommandReport(rejected []string) {
	if len(rejected) == 0 || m.commandReportShown {
		return
	}
	m.commandReportShown = true
	m.Status = "部分自定义命令未加载：" + strings.Join(rejected, "；")
}

// registerBuiltins registers the built-in commands on a fresh registry. The
// Local closures act through the command host and replicate the historical
// hardcoded submitComposer branches one to one, so dispatching through the
// registry keeps user-visible behavior unchanged.
func registerBuiltins(host *commandHost, registry *commands.Registry) {
	set := func(name, description, argPrompt string, local func(args string)) {
		registry.Register(&commands.Command{Name: name, Description: description, ArgPrompt: argPrompt, Kind: commands.KindLocal, Local: local})
	}
	set("sessions", "浏览会话", "", func(string) {
		m := host.model
		m.Composer.SetValue("")
		m.Navigation = NavigationState{Mode: SessionPickerView, Cursor: m.sessionIndex()}
	})
	set("goals", "浏览目标", "", func(string) {
		m := host.model
		m.Composer.SetValue("")
		m.Navigation = NavigationState{Mode: GoalPickerView, Cursor: m.SelectedGoal}
	})
	set("search", "搜索会话内容", "关键词", func(args string) {
		m := host.model
		query := strings.TrimSpace(args)
		if query == "" {
			m.Status = "用法：/search 关键词"
			return
		}
		m.Pending = true
		m.Status = "正在搜索会话…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "session_search", ProjectRoot: m.Root, Text: query}))
	})
	set("review", "预览候选变更", "候选ID", func(args string) {
		m := host.model
		candidateID := strings.TrimSpace(args)
		if candidateID == "" || strings.ContainsAny(candidateID, " \t\n") {
			m.Status = "用法：/review 候选ID"
			return
		}
		m.Pending = true
		m.Status = "正在生成候选预览…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "review_get", CandidateID: candidateID, SessionID: m.ActiveSession}))
	})
	set("say", "为目标排队补充指令", "补充指令", func(args string) {
		m := host.model
		sayText := strings.TrimSpace(args)
		if sayText == "" {
			m.Status = "用法：/say 补充指令"
			return
		}
		if len(m.Goals) == 0 {
			m.Status = "先用 Ctrl+G 选择目标，再 /say。"
			return
		}
		req := conversation.ClientMsg{Op: "say", ProjectRoot: m.Root, SessionID: m.ActiveSession, Goal: m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID, Text: sayText}
		m.Pending = true
		m.Status = "正在排队补充指令…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, req))
	})
	set("reply", "答复待答问题", "答复内容", func(args string) {
		m := host.model
		replyText := strings.TrimSpace(args)
		if replyText == "" {
			m.Status = "用法：/reply 答复内容"
			return
		}
		question, ok := m.pendingQuestion()
		if !ok {
			m.Status = "当前没有待回答的问题。"
			return
		}
		req := conversation.ClientMsg{Op: "reply", SessionID: m.ActiveSession, QuestionID: question.QuestionID, Text: replyText}
		m.Pending = true
		m.Status = "正在记录答复…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, req))
	})
	set("confirm", "确认待定提案", "提案ID", func(args string) {
		m := host.model
		id := strings.TrimSpace(args)
		if id == "" || strings.ContainsAny(id, " \t\n") {
			m.Status = "用法：/confirm 提案ID"
			return
		}
		m.Pending = true
		m.Status = "正在记录提案确认…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "confirm", ID: id, SessionID: m.ActiveSession}))
	})
	set("reject", "拒绝待定提案", "提案ID", func(args string) {
		m := host.model
		id := strings.TrimSpace(args)
		if id == "" || strings.ContainsAny(id, " \t\n") {
			m.Status = "用法：/reject 提案ID"
			return
		}
		m.Pending = true
		m.Status = "正在记录提案拒绝…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "reject", ID: id, SessionID: m.ActiveSession}))
	})
	// /plan only constructs the plan_mode op here; the service grows the
	// handler in a later task, and until then the request fails through the
	// ordinary error path, which is the intended placeholder behavior.
	set("plan", "切换计划模式", "", func(string) {
		m := host.model
		m.Pending = true
		m.Status = "正在切换计划模式…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "plan_mode", SessionID: m.ActiveSession}))
	})
	set("help", "显示可用命令", "", func(string) {
		m := host.model
		m.Composer.SetValue("")
		m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: renderHelp(m.registry.List()), Kind: "text"}})
		m.Transcript.SetEvents(m.Events)
	})
	set("skills", "列出可用技能", "reload", func(args string) {
		m := host.model
		m.Composer.SetValue("")
		if strings.TrimSpace(args) == "reload" {
			if m.skills == nil {
				m.Status = "技能目录不可用。"
				return
			}
			before := len(m.skills.List())
			m.skills.Reload()
			after := len(m.skills.List())
			m.Pending = true
			m.Status = fmt.Sprintf("技能已重载：%d → %d。", before, after)
			m.recordHistory(host.raw)
			host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "skill_reload", SessionID: m.ActiveSession}))
			return
		}
		if m.skills == nil {
			m.Status = "技能目录不可用。"
			return
		}
		list := m.skills.List()
		var b strings.Builder
		b.WriteString("可用技能：")
		for _, s := range list {
			fmt.Fprintf(&b, "\n- /%s — %s（来源：%s）", s.Meta.Name, s.Meta.Description, s.Source)
		}
		if len(list) == 0 {
			b.WriteString("\n（无）\n提示：把技能放到项目的 .stable/skills/<名称>/SKILL.md 或用户级 ~/.config/stable/skills/<名称>/SKILL.md。")
		}
		m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: b.String(), Kind: "text"}})
		m.Transcript.SetEvents(m.Events)
		if m.ActiveSession != "" {
			m.Pending = true
			host.send(requestCmd(m.Socket, conversation.ClientMsg{Op: "skill_list", SessionID: m.ActiveSession}))
		}
	})
}

// renderHelp formats the merged command list for the /help transcript note;
// commands that document an argument prompt show it as a hint.
func renderHelp(cmds []*commands.Command) string {
	var b strings.Builder
	b.WriteString("可用命令：")
	for _, c := range cmds {
		fmt.Fprintf(&b, "\n- /%s %s", c.Name, c.Description)
		if c.ArgPrompt != "" {
			fmt.Fprintf(&b, "（参数：%s）", c.ArgPrompt)
		}
	}
	return b.String()
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

// DialogKind identifies the modal layer that currently claims the keyboard
// and the view. Zero means no dialog is pending.
type DialogKind uint8

const (
	DialogNone DialogKind = iota
	DialogApproval
	DialogQuestion
	DialogPlan
	DialogReview
	DialogProposal
)

// pendingDialog returns the dialog that should claim the keyboard and the
// view right now, in the decision-queue order of spec AC7: permission
// approvals first, then pending questions, plan approvals, the user-opened
// candidate review, and finally goal proposals. Both the Update key dispatch
// and the View render short-circuit through this function, so closing the
// top layer automatically drops the next one into place.
func (m Model) pendingDialog() DialogKind {
	if len(m.Approvals) > 0 {
		return DialogApproval
	}
	if _, ok := m.popupQuestion(); ok {
		return DialogQuestion
	}
	if _, ok := m.activePlanApproval(); ok {
		return DialogPlan
	}
	if m.Review != nil {
		return DialogReview
	}
	if _, ok := m.popupProposal(); ok {
		return DialogProposal
	}
	return DialogNone
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = v.Width, v.Height
		m.resize()
		return m, nil
	case approvalPollTick:
		if m.ActiveSession == "" {
			return m, approvalPollCmd()
		}
		return m, tea.Batch(requestCmd(m.Socket, conversation.ClientMsg{Op: "approval_list", SessionID: m.ActiveSession}), approvalPollCmd())
	case resultMsg:
		return m.handleResult(v)
	case runStreamStartedMsg:
		if v.err != nil {
			m.Pending = false
			m.ActiveRunID = ""
			m.Err, m.Status = v.err, "请求失败："+v.err.Error()
			if m.ActiveSession != "" {
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
			}
			return m, nil
		}
		m.stream = v.client
		if !v.resume {
			m.Pending = true
		}
		return m, receiveRunCmd(v.client)
	case runStreamMsg:
		if v.err != nil {
			if m.stream != nil {
				_ = m.stream.Close()
				m.stream = nil
			}
			m.Err, m.Status = v.err, "连接中断，正在恢复运行流…"
			if m.ActiveRunID != "" {
				return m, resumeRunCmd(m.Socket, m.ActiveSession, m.ActiveRunID, m.LastCursor)
			}
			m.Pending = false
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
		if v.message.Type == "cancel_sent" {
			return m, nil
		}
		if v.message.Type == "error" {
			m.Err, m.Status = fmt.Errorf("%s", v.message.Error), "请求失败："+v.message.Error
			_ = v.client.Close()
			m.stream = nil
			m.Pending = false
			m.ActiveRunID = ""
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
		if v.message.Type == "resubscribed" {
			return m, receiveRunCmd(v.client)
		}
		if m.Pending && m.ActiveRunID != "" && v.message.RunID != "" && v.message.RunID != m.ActiveRunID {
			return m, receiveRunCmd(v.client)
		}
		m.applyRunMessage(v.message)
		if v.message.Type == "resync" {
			return m, resubscribeRunCmd(v.client, m.ActiveSession, v.message.RunID, v.message.Cursor)
		}
		if v.message.Type == "run_outcome" {
			m.Pending = false
			if v.message.Outcome != nil {
				switch v.message.Outcome.Status {
				case agent.RunCompleted:
					m.Status = "回答完成。"
				case agent.RunCancelled:
					m.Status = "已取消；已收到的内容已保留。"
				case agent.RunAwaitingTools:
					m.Status = "模型请求了工具；当前阶段未执行工具。"
				case agent.RunFailed:
					m.Status = "运行失败；已收到的内容已保留。"
				}
			}
			_ = v.client.Close()
			m.stream = nil
			m.ActiveRunID = ""
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
		return m, receiveRunCmd(v.client)
	case tea.KeyMsg:
		if v.String() == "ctrl+c" {
			if m.stream != nil && m.ActiveRunID != "" {
				_ = m.stream.Cancel(m.ActiveSession, m.ActiveRunID)
			}
			return m, tea.Quit
		}
		if m.Navigation.Mode != ChatView {
			return m.handleNavigationKey(v)
		}
		// The pending decision queue claims the keyboard in priority order
		// (AC7): closing or resolving the top layer drops the next one into
		// place on the following key.
		switch m.pendingDialog() {
		case DialogApproval:
			return m.handleApprovalKey(v)
		case DialogQuestion:
			return m.handleQuestionKey(v)
		case DialogPlan:
			return m.handlePlanKey(v)
		case DialogReview:
			return m.handleReviewKey(v)
		case DialogProposal:
			return m.handleProposalKey(v)
		}
		return m.handleChatKey(v)
	}
	return m, nil
}
func (m *Model) resize() {
	m.Layout = ComputeLayout(m.Width, m.Height, m.Composer.Height())
	m.Composer.SetSize(max(1, m.Width-4), max(1, min(6, m.Height/3)))
	m.Layout = ComputeLayout(m.Width, m.Height, m.Composer.Height())
	m.Transcript.SetSize(m.Layout.TranscriptWidth, m.Layout.TranscriptHeight)
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
		case "review":
			if x.Review != nil {
				m.Review = x.Review
				m.ReviewCandidate = x.Review.CandidateID
				m.ReviewConfirmed = map[string]bool{}
				m.ReviewCursor = 0
				m.ReviewSnapshots = nil
				m.RewindPick, m.RewindArmed = false, false
				m.Status = "候选预览已载入：a 普通接收，f 强制接收（逐项确认），r 回滚到快照，Esc 关闭。"
			}
		case "snapshots":
			m.ReviewSnapshots = append([]sessionlog.SnapshotRef(nil), x.Snapshots...)
		case "rewind":
			if x.Rewind != nil {
				m.RewindPick, m.RewindArmed = false, false
				if x.Rewind.Status == sessionlog.RewindFailed {
					m.Status = "回滚失败：" + x.Rewind.Error
				} else {
					m.Status = "已回滚到快照 " + x.Rewind.SnapshotID + "，快照之后创建的内容已移除。"
				}
			}
		case "questions":
			// A question_list result is the full session list (session
			// restore or post-reply refresh): it replaces the stored set and
			// never marks questions live, so a restored session stays quiet.
			// The same message type interleaved into another request while a
			// run blocks inside ask_user is a live arrival and opens the
			// dialog through the decision queue.
			m.applyQuestions(x.Questions, r.op == "question_list")
		case "reply":
			if x.Reply != nil {
				m.Status = "答复已记录。"
				if m.questionDismissed == nil {
					m.questionDismissed = map[string]bool{}
				}
				m.questionDismissed[x.Reply.QuestionID] = true
				delete(m.liveQuestions, x.Reply.QuestionID)
			}
		case "todo":
			// The transcript projection renders todo snapshots; the TUI only
			// stores the latest list here.
			m.Todos = append([]sessionlog.TaskSnapshot(nil), x.Tasks...)
		case "skill_report":
			if x.SkillReport != nil {
				switch x.SkillReport.Kind {
				case conversation.SkillReportError:
					m.Status = "技能调用失败：" + x.SkillReport.Error
				case conversation.SkillReportReload:
					m.Status = fmt.Sprintf("服务端技能已重载：%d → %d。", x.SkillReport.Before, x.SkillReport.After)
				case conversation.SkillReportDelta:
					m.Status = "新增可用技能：" + strings.Join(x.SkillReport.Added, "、")
				}
			}
		case "skill_list":
			if len(x.SkillActivated) > 0 {
				m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: "本会话已激活技能：" + strings.Join(x.SkillActivated, "、"), Kind: "text"}})
				m.Transcript.SetEvents(m.Events)
			} else {
				m.Status = "当前会话没有已激活技能。"
			}
		case "plan_state":
			if x.PlanState != nil {
				m.Plan = x.PlanState
			}
		case "plan_approval_pending", "plan_approvals":
			for _, ref := range x.PlanApprovals {
				m.upsertPlanApproval(ref)
			}
		case "plan_approval_resolved":
			m.clearPlanApprovals()
			if x.PlanState != nil {
				m.Plan = x.PlanState
			}
		case "search":
			if x.Search != nil {
				m.SearchHits = x.Search.Hits
				m.SearchCorrupt = x.Search.Corrupt
				m.Navigation = NavigationState{Mode: SearchResultsView, Cursor: 0}
				if len(m.SearchHits) == 0 {
					m.Status = "没有匹配的会话。"
				} else {
					m.Status = "Enter 恢复选中的会话。"
				}
			}
		case "acceptance":
			m.Review = nil
			m.ReviewConfirmed = nil
			m.Status = "候选已接收，目标仍需独立复核。"
		case "error":
			m.Err = fmt.Errorf("%s", x.Error)
			m.Status = "请求未完成。"
		case "approvals":
			m.Approvals = append([]permission.ApprovalPrompt(nil), x.Approvals...)
			if m.SelectedApproval >= len(m.Approvals) {
				m.SelectedApproval = max(0, len(m.Approvals)-1)
			}
		case "permission_decision", "approval_cancelled":
			id := ""
			if x.Decision != nil {
				id = x.Decision.ApprovalID
			}
			if x.Approval != nil {
				id = x.Approval.ID
			}
			m.removeApproval(id)
			if x.Decision != nil {
				m.Status = "授权决定已记录：" + string(x.Decision.Kind)
			}
		case "approval_pending":
			if x.Approval != nil {
				m.upsertApproval(*x.Approval)
			}
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
				m.LastCursor = 0
				for _, event := range m.Events {
					if event.Seq > m.LastCursor {
						m.LastCursor = event.Seq
					}
				}
			}
			if x.Goals != nil {
				m.Goals = x.Goals
			}
			// Plan mode is session runtime state echoed by session_load; the
			// status line shows it, nothing else depends on it here.
			if x.Plan != nil {
				m.Plan = x.Plan
			}
			// Proposals rebuilt from the transcript are restore state: they
			// never open the proposal dialog (spec F6), /confirm and /reject
			// stay the way to handle them.
			m.Proposals = proposalsFromEvents(m.Events)
			if m.Err == nil {
				m.Status = "会话已加载。"
			}
			m.Transcript.SetEvents(m.Events)
		case "goal_update":
			if x.Goal != nil {
				m.upsertGoal(*x.Goal)
			}
		case "proposal":
			if x.Proposal != nil {
				m.upsertProposal(*x.Proposal)
				if m.liveProposals == nil {
					m.liveProposals = map[string]bool{}
				}
				m.liveProposals[x.Proposal.ID] = true
			}
		}
	}
	if r.op == "chat" || r.op == "create_goal" || r.op == "confirm" || r.op == "reject" {
		if m.ActiveSession != "" {
			m.Pending = true
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
	}
	// A /say success means the instruction is persisted and queued for the
	// goal's next decision round. The protocol does not yet report the exact
	// consumption moment, so the UI shows the durable queued state only and
	// never claims the instruction was consumed.
	if r.op == "say" {
		m.Status = "已排队：下一轮决策按序消费。"
		return m, nil
	}
	if r.op == "review_get" && m.Review != nil {
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "snapshot_list", SessionID: m.ActiveSession, CandidateID: m.Review.CandidateID})
	}
	if r.op == "snapshot_rewind" && m.Review != nil {
		// The rewind moved the candidate back; the open review is stale.
		m.Pending = true
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "review_get", CandidateID: m.Review.CandidateID, SessionID: m.ActiveSession})
	}
	if r.op == "reply" {
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "question_list", SessionID: m.ActiveSession})
	}
	if r.op == "plan_resolve" {
		// The resolve response reports the new plan state; the resolved
		// broadcast may never reach a one-shot connection, so the pending
		// plan dialog is dropped here as well.
		m.clearPlanApprovals()
	}
	if r.op == "session_load" && m.ActiveSession != "" {
		list := requestCmd(m.Socket, conversation.ClientMsg{Op: "approval_list", SessionID: m.ActiveSession})
		questions := requestCmd(m.Socket, conversation.ClientMsg{Op: "question_list", SessionID: m.ActiveSession})
		if !m.approvalPollStarted {
			m.approvalPollStarted = true
			return m, tea.Batch(list, questions, approvalPollCmd())
		}
		return m, tea.Batch(list, questions)
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

// applyQuestions stores one questions update. fullList marks a question_list
// result — the complete session list, so it replaces the stored set and never
// marks questions live (restore and post-reply refreshes stay quiet). A live
// arrival upserts into the stored set and marks the pending questions live so
// the decision queue opens the dialog.
func (m *Model) applyQuestions(questions []sessionlog.PendingQuestion, fullList bool) {
	if fullList {
		m.Questions = append([]sessionlog.PendingQuestion(nil), questions...)
	} else {
		for _, q := range questions {
			m.upsertQuestion(q)
			if q.Status == sessionlog.QuestionPending {
				if m.liveQuestions == nil {
					m.liveQuestions = map[string]bool{}
				}
				m.liveQuestions[q.QuestionID] = true
			}
		}
	}
	pending := map[string]bool{}
	for _, q := range m.Questions {
		if q.Status == sessionlog.QuestionPending {
			pending[q.QuestionID] = true
		}
	}
	for id := range m.liveQuestions {
		if !pending[id] {
			delete(m.liveQuestions, id)
		}
	}
	// The dialog state belongs to one question; a different popup starts
	// clean so toggles never leak across questions.
	if q, ok := m.popupQuestion(); ok && q.QuestionID != m.questionDialogID {
		m.resetQuestionDialog()
		m.questionDialogID = q.QuestionID
	}
}

func (m *Model) upsertQuestion(q sessionlog.PendingQuestion) {
	for i := range m.Questions {
		if m.Questions[i].QuestionID == q.QuestionID {
			m.Questions[i] = q
			return
		}
	}
	m.Questions = append(m.Questions, q)
}

func (m *Model) upsertPlanApproval(ref conversation.PlanApprovalRef) {
	for i := range m.PlanApprovals {
		if m.PlanApprovals[i].ID == ref.ID {
			m.PlanApprovals[i] = ref
			return
		}
	}
	m.PlanApprovals = append(m.PlanApprovals, ref)
	// A fresh approval opens the dialog from the first choice.
	m.SelectedPlan, m.PlanFeedback = 0, ""
}

// clearPlanApprovals drops the pending plan approvals of the active session;
// plan_resolve answers the session's single pending request.
func (m *Model) clearPlanApprovals() {
	kept := make([]conversation.PlanApprovalRef, 0, len(m.PlanApprovals))
	for _, approval := range m.PlanApprovals {
		if approval.SessionID != m.ActiveSession {
			kept = append(kept, approval)
		}
	}
	m.PlanApprovals = kept
}
func (m *Model) upsertApproval(a permission.ApprovalPrompt) {
	for i := range m.Approvals {
		if m.Approvals[i].ID == a.ID {
			m.Approvals[i] = a
			return
		}
	}
	m.Approvals = append(m.Approvals, a)
}
func (m *Model) removeApproval(id string) {
	for i := range m.Approvals {
		if m.Approvals[i].ID == id {
			m.Approvals = append(m.Approvals[:i], m.Approvals[i+1:]...)
			if m.SelectedApproval >= len(m.Approvals) {
				m.SelectedApproval = max(0, len(m.Approvals)-1)
			}
			return
		}
	}
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
func (m Model) sessionIndex() int {
	for i, s := range m.Sessions {
		if s.ID == m.ActiveSession {
			return i
		}
	}
	return 0
}
func (m *Model) SelectedSession(delta int) {
	i := clamp(m.sessionIndex()+delta, 0, len(m.Sessions)-1)
	if len(m.Sessions) > 0 {
		m.ActiveSession = m.Sessions[i].ID
	}
}

func (m Model) handleChatKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.Mode != "" {
		return m.handleInput(k)
	}
	switch k.String() {
	case "ctrl+s":
		m.Navigation = NavigationState{Mode: SessionPickerView, Cursor: m.sessionIndex()}
		return m, nil
	case "ctrl+g":
		m.Navigation = NavigationState{Mode: GoalPickerView, Cursor: m.SelectedGoal}
		return m, nil
	case "ctrl+p":
		return m.historyOlder()
	case "ctrl+n":
		return m.historyNewer()
	case "pgup", "ctrl+up":
		var cmd tea.Cmd
		m.Transcript.Viewport, cmd = m.Transcript.Viewport.Update(k)
		return m, cmd
	case "pgdown", "ctrl+down":
		var cmd tea.Cmd
		m.Transcript.Viewport, cmd = m.Transcript.Viewport.Update(k)
		return m, cmd
	case "up", "k":
		if len(m.Candidates) > 0 {
			m.CandidateIndex = (m.CandidateIndex - 1 + len(m.Candidates)) % len(m.Candidates)
			return m, nil
		}
		if m.Pending && len(m.Sessions) > 0 {
			m.SelectedSession(-1)
			return m, nil
		}
		return m, m.Composer.Update(k)
	case "down", "j":
		if len(m.Candidates) > 0 {
			m.CandidateIndex = (m.CandidateIndex + 1) % len(m.Candidates)
			return m, nil
		}
		if m.Pending && len(m.Sessions) > 1 {
			m.SelectedSession(1)
			return m, nil
		}
		return m, m.Composer.Update(k)
	case "esc":
		if m.Pending && m.stream != nil && m.ActiveRunID != "" {
			m.Status = "正在取消运行…"
			return m, cancelRunCmd(m.stream, m.ActiveSession, m.ActiveRunID)
		}
		m.Composer.Blur()
		m.Mode = ""
		m.Status = ""
		return m, nil
	case "ctrl+j":
		m.Composer.SetValue(m.Composer.Value() + "\n")
		return m, nil
	case "tab":
		m.refreshCompletions()
		if len(m.Candidates) > 0 {
			item := m.Candidates[m.CandidateIndex%len(m.Candidates)]
			value := m.Composer.Value()
			if item.Kind == CommandCompletion {
				m.Composer.SetValue(item.InsertText)
			} else {
				at := strings.LastIndex(value, "@")
				if at >= 0 {
					m.Composer.SetValue(value[:at] + item.InsertText)
				}
			}
			m.Candidates = nil
			return m, nil
		}
		return m, nil
	case "enter":
		return m.submitComposer()
	default:
		cmd := m.Composer.Update(k)
		m.refreshCompletions()
		return m, cmd
	}
}

func (m Model) handleInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "esc" {
		m.Mode = ""
		m.Composer.Blur()
		m.Status = "已取消输入。"
		return m, nil
	}
	if k.String() == "ctrl+p" {
		return m.historyOlder()
	}
	if k.String() == "ctrl+n" {
		return m.historyNewer()
	}
	if k.String() == "ctrl+j" {
		m.Composer.SetValue(m.Composer.Value() + "\n")
		return m, nil
	}
	if k.String() == "tab" {
		m.refreshCompletions()
		if len(m.Candidates) > 0 {
			item := m.Candidates[m.CandidateIndex%len(m.Candidates)]
			value := m.Composer.Value()
			if item.Kind == CommandCompletion {
				m.Composer.SetValue(item.InsertText)
			} else if at := strings.LastIndex(value, "@"); at >= 0 {
				m.Composer.SetValue(value[:at] + item.InsertText)
			}
			m.Candidates = nil
		}
		return m, nil
	}
	if k.String() == "enter" {
		return m.submitComposer()
	}
	cmd := m.Composer.Update(k)
	m.refreshCompletions()
	return m, cmd
}

func (m *Model) refreshCompletions() {
	value := m.Composer.Value()
	if strings.HasPrefix(value, "/") && !strings.ContainsAny(value, " \n") {
		m.refreshCommands()
		m.Candidates = FilterCompletions(CommandItems(m.registry.List()), value)
		return
	}
	if at := strings.LastIndex(value, "@"); at >= 0 && !strings.ContainsAny(value[at:], " \n") {
		items, _ := (projectPathCompleter{}).Complete(m.Root, value[at:])
		m.Candidates = items
		return
	}
	m.Candidates = nil
}
func (m Model) submitComposer() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.Composer.Value())
	if text == "" {
		m.Status = "请输入非空内容。"
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		if model, cmd, handled := m.dispatchCommand(text); handled {
			return model, cmd
		}
	}
	if text == "/sessions" {
		m.Composer.SetValue("")
		m.Navigation = NavigationState{Mode: SessionPickerView, Cursor: m.sessionIndex()}
		return m, nil
	}
	if text == "/goals" {
		m.Composer.SetValue("")
		m.Navigation = NavigationState{Mode: GoalPickerView, Cursor: m.SelectedGoal}
		return m, nil
	}
	if strings.HasPrefix(text, "/review ") {
		candidateID := strings.TrimSpace(strings.TrimPrefix(text, "/review "))
		if candidateID == "" || strings.ContainsAny(candidateID, " \t\n") {
			m.Status = "用法：/review 候选ID"
			return m, nil
		}
		m.Pending = true
		m.Status = "正在生成候选预览…"
		m.Composer.SetValue("")
		m.recordHistory(text)
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "review_get", CandidateID: candidateID, SessionID: m.ActiveSession})
	}
	if text == "/search" || strings.HasPrefix(text, "/search ") {
		query := strings.TrimSpace(strings.TrimPrefix(text, "/search"))
		if query == "" {
			m.Status = "用法：/search 关键词"
			return m, nil
		}
		m.Pending = true
		m.Status = "正在搜索会话…"
		m.Composer.SetValue("")
		m.recordHistory(text)
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_search", ProjectRoot: m.Root, Text: query})
	}
	if text == "/say" || strings.HasPrefix(text, "/say ") {
		sayText := strings.TrimSpace(strings.TrimPrefix(text, "/say"))
		if sayText == "" {
			m.Status = "用法：/say 补充指令"
			return m, nil
		}
		if len(m.Goals) == 0 {
			m.Status = "先用 Ctrl+G 选择目标，再 /say。"
			return m, nil
		}
		req := conversation.ClientMsg{Op: "say", ProjectRoot: m.Root, SessionID: m.ActiveSession, Goal: m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID, Text: sayText}
		m.Pending = true
		m.Status = "正在排队补充指令…"
		m.Composer.SetValue("")
		m.recordHistory(text)
		return m, requestCmd(m.Socket, req)
	}
	if text == "/reply" || strings.HasPrefix(text, "/reply ") {
		replyText := strings.TrimSpace(strings.TrimPrefix(text, "/reply"))
		if replyText == "" {
			m.Status = "用法：/reply 答复内容"
			return m, nil
		}
		question, ok := m.pendingQuestion()
		if !ok {
			m.Status = "当前没有待回答的问题。"
			return m, nil
		}
		req := conversation.ClientMsg{Op: "reply", SessionID: m.ActiveSession, QuestionID: question.QuestionID, Text: replyText}
		m.Pending = true
		m.Status = "正在记录答复…"
		m.Composer.SetValue("")
		m.recordHistory(text)
		return m, requestCmd(m.Socket, req)
	}
	return m.submitChatPath(text, text)
}

// submitChatPath is the shared submission tail of the composer: text goes
// through the ordinary request path (chat opens a run; other ops go through
// requestCmd). history is the line recorded in the input history, which lets
// expanded prompt commands record the typed command instead of its expansion.
func (m Model) submitChatPath(text, history string) (tea.Model, tea.Cmd) {
	req := conversation.ClientMsg{ProjectRoot: m.Root, SessionID: m.ActiveSession, Text: text}
	if len(m.Goals) > 0 && m.Navigation.Mode == GoalPickerView {
		req.Goal = m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID
	}
	switch m.Mode {
	case GoalInput:
		req.Op = "create_goal"
	case SayInput:
		req.Op = "say"
	case ReplyInput:
		req.Op = "reply"
	default:
		req.Op = "chat"
	}
	if (req.Op == "say" || req.Op == "reply") && len(m.Goals) > 0 {
		req.Goal = m.Goals[clamp(m.SelectedGoal, 0, len(m.Goals)-1)].ID
	}
	m.Pending = true
	m.Status = "请求已提交。"
	m.Err = nil
	m.Mode = ""
	m.Composer.SetValue("")
	m.Composer.Blur()
	m.recordHistory(history)
	if req.Op == "chat" {
		runID, err := sessionlog.NewID()
		if err != nil {
			m.Status, m.Pending = "无法创建运行 ID："+err.Error(), false
			return m, nil
		}
		request := agent.ExecutionRequest{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: m.ActiveSession}, Intent: text, Messages: []llm.Message{{Role: "user", Content: text}}}
		m.ActiveRunID = runID
		m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "user", Text: text, Kind: "text"}})
		m.Transcript.SetEvents(m.Events)
		m.Status = "正在连接模型…"
		return m, openRunCmd(m.Socket, request)
	}
	return m, requestCmd(m.Socket, req)
}

// dispatchCommand runs a registered slash command and reports whether the
// input was handled. Input without a command name and unknown commands
// ("/xyz") return handled=false so the ordinary submission path keeps its
// historical behavior. KindLocal commands run their closure in place;
// KindPrompt commands expand and re-enter the ordinary chat path.
func (m Model) dispatchCommand(text string) (tea.Model, tea.Cmd, bool) {
	name, args := commands.Parse(text)
	if name == "" {
		return m, nil, false
	}
	m.refreshCommands()
	cmd, ok := m.registry.Find(name)
	if !ok {
		return m, nil, false
	}
	if cmd.Kind == commands.KindPrompt {
		m.Candidates = nil
		model, submit := m.submitChatPath(commands.ExpandPrompt(cmd.Body, args), text)
		return model, submit, true
	}
	m.host.model, m.host.raw, m.host.cmds = &m, text, nil
	cmd.Local(args)
	m.Candidates = nil
	queued := m.host.cmds
	m.host.model, m.host.raw, m.host.cmds = nil, "", nil
	switch len(queued) {
	case 0:
		return m, nil, true
	case 1:
		return m, queued[0], true
	default:
		return m, tea.Batch(queued...), true
	}
}

// recordHistory appends one submitted input to the project history. The
// submission itself never depends on the history file, but a failed append
// must be visible instead of silently losing the entry.
func (m *Model) recordHistory(text string) {
	m.histCursor, m.histDraft = nil, ""
	if m.history == nil {
		if m.historyErr != nil {
			m.Status = "输入历史未保存：" + m.historyErr.Error()
		}
		return
	}
	if _, err := m.history.Append(text); err != nil {
		m.Status = "输入历史未保存：" + err.Error()
	}
}

// pendingQuestion returns the oldest still-unanswered question in the active
// session, which is the only one /reply is allowed to target.
func (m Model) pendingQuestion() (sessionlog.PendingQuestion, bool) {
	for _, q := range m.Questions {
		if q.Status == sessionlog.QuestionPending {
			return q, true
		}
	}
	return sessionlog.PendingQuestion{}, false
}

// historyOlder/Newer walk the persisted input history with Ctrl+P/Ctrl+N.
// Navigation never submits anything; the live draft is saved on entry and
// restored when the user steps back past the newest entry.
func (m Model) historyOlder() (tea.Model, tea.Cmd) {
	if m.history == nil {
		if m.historyErr != nil {
			m.Status = "输入历史不可用：" + m.historyErr.Error()
		}
		return m, nil
	}
	if m.histCursor == nil {
		cursor, err := m.history.Cursor()
		if err != nil {
			m.Status = "输入历史不可用：" + err.Error()
			return m, nil
		}
		m.histCursor = cursor
		m.histDraft = m.Composer.Value()
	}
	entry, ok := m.histCursor.Prev()
	if !ok {
		m.Status = "已到最早一条历史。"
		return m, nil
	}
	m.Composer.SetValue(entry.Text)
	return m, nil
}

func (m Model) historyNewer() (tea.Model, tea.Cmd) {
	if m.histCursor == nil {
		return m, nil
	}
	entry, ok := m.histCursor.Next()
	if !ok {
		m.Composer.SetValue(m.histDraft)
		m.histCursor, m.histDraft = nil, ""
		return m, nil
	}
	m.Composer.SetValue(entry.Text)
	return m, nil
}

func (m Model) handleReviewKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.RewindPick {
		return m.handleRewindPickKey(k)
	}
	switch k.String() {
	case "esc":
		m.Review, m.ReviewConfirmed, m.ReviewSnapshots = nil, nil, nil
		m.Status = "已关闭候选预览。"
		return m, nil
	case "r":
		// Rewind targets a snapshot of this candidate; an active run could be
		// writing to it, so the picker stays disabled with an explicit reason.
		if m.ActiveRunID != "" || m.Pending {
			m.Status = "有活动运行：请先等待运行结束或按 Esc 取消运行，再回滚。"
			return m, nil
		}
		if len(m.ReviewSnapshots) == 0 {
			m.Status = "该候选在此会话没有可回滚的快照。"
			return m, nil
		}
		m.RewindPick, m.RewindCursor, m.RewindArmed = true, 0, false
		m.Status = "选择回滚目标快照。"
		return m, nil
	case " ":
		if len(m.Review.Findings) > 0 {
			finding := m.Review.Findings[clamp(m.ReviewCursor, 0, len(m.Review.Findings)-1)]
			if finding.Result != candidate.FindingPass {
				m.ReviewConfirmed[finding.ID] = !m.ReviewConfirmed[finding.ID]
			}
		}
		return m, nil
	case "right", "l":
		if m.ReviewCursor < len(m.Review.Findings)-1 {
			m.ReviewCursor++
		}
		return m, nil
	case "left", "h":
		if m.ReviewCursor > 0 {
			m.ReviewCursor--
		}
		return m, nil
	case "a":
		m.Pending, m.Status = true, "正在核对并接收候选…"
		return m, m.acceptReview(candidate.AcceptNormal)
	case "f":
		var confirmed []string
		for _, finding := range m.Review.Findings {
			if finding.Result != candidate.FindingPass {
				if !m.ReviewConfirmed[finding.ID] {
					m.Status = "强制接收前需逐项确认所有失败或不可用项；Space 确认当前项。"
					return m, nil
				}
				confirmed = append(confirmed, finding.ID)
			}
		}
		m.Pending, m.Status = true, "正在核对并强制接收候选…"
		return m, m.acceptReview(candidate.AcceptForce, confirmed...)
	}
	return m, nil
}

// handleRewindPickKey drives the two-step rewind confirmation inside the
// review dialog: the first Enter arms a concrete target snapshot, the second
// sends the rewind request. The request carries the digest from the loaded
// review, so a candidate that moved in between is refused server-side.
func (m Model) handleRewindPickKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.RewindPick, m.RewindArmed = false, false
		m.Status = "已取消回滚选择。"
		return m, nil
	case "up", "k", "left", "h":
		if m.RewindCursor > 0 {
			m.RewindCursor--
			m.RewindArmed = false
		}
		return m, nil
	case "down", "j", "right", "l":
		if m.RewindCursor < len(m.ReviewSnapshots)-1 {
			m.RewindCursor++
			m.RewindArmed = false
		}
		return m, nil
	case "enter":
		if len(m.ReviewSnapshots) == 0 || m.Review == nil {
			m.RewindPick, m.RewindArmed = false, false
			return m, nil
		}
		target := m.ReviewSnapshots[clamp(m.RewindCursor, 0, len(m.ReviewSnapshots)-1)]
		if !m.RewindArmed {
			m.RewindArmed = true
			m.Status = "再次 Enter 确认回滚到快照 " + shortDigest(target.Digest) + "。"
			return m, nil
		}
		m.Pending = true
		m.RewindPick, m.RewindArmed = false, false
		m.Status = "正在回滚候选到快照…"
		return m, requestCmd(m.Socket, conversation.ClientMsg{
			Op: "snapshot_rewind", SessionID: m.ActiveSession, CandidateID: m.Review.CandidateID,
			SnapshotID: target.SnapshotID, CandidateDigest: m.Review.CandidateDigest,
		})
	}
	return m, nil
}

func (m Model) handleApprovalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.Approvals) == 0 {
		return m, nil
	}
	a := m.Approvals[clamp(m.SelectedApproval, 0, len(m.Approvals)-1)]
	switch k.String() {
	case "esc":
		return m, nil
	case "up", "k":
		if m.SelectedApproval > 0 {
			m.SelectedApproval--
		}
		return m, nil
	case "down", "j":
		if m.SelectedApproval < len(m.Approvals)-1 {
			m.SelectedApproval++
		}
		return m, nil
	case "1", "2", "3":
		choice, err := approvalChoice(k.String())
		if err != nil {
			m.Err = err
			return m, nil
		}
		m.Pending = true
		m.Status = "正在记录授权决定…"
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "approval_resolve", SessionID: m.ActiveSession, ApprovalID: a.ID, ApprovalChoice: string(choice)})
	case "c":
		m.Pending = true
		m.Status = "正在取消授权请求…"
		return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "approval_cancel", SessionID: m.ActiveSession, ApprovalID: a.ID})
	}
	return m, nil
}

func approvalChoice(key string) (permission.ApprovalChoice, error) {
	switch key {
	case "1":
		return permission.ChoiceAllowOnce, nil
	case "2":
		return permission.ChoiceSaveRule, nil
	case "3":
		return permission.ChoiceDeny, nil
	default:
		return "", fmt.Errorf("unknown approval choice")
	}
}

func (m Model) acceptReview(mode candidate.AcceptanceMode, confirmed ...string) tea.Cmd {
	decisionID, err := sessionlog.NewID()
	if err != nil {
		return func() tea.Msg { return resultMsg{op: "review_accept", err: err} }
	}
	return requestCmd(m.Socket, conversation.ClientMsg{Op: "review_accept", CandidateID: m.Review.CandidateID, DecisionID: decisionID, PreviewDigest: m.Review.Digest, CandidateDigest: m.Review.CandidateDigest, FormalDigest: m.Review.FormalDigest, AcceptanceMode: string(mode), Confirmed: confirmed})
}

func (m *Model) applyRunMessage(message conversation.ServerMsg) {
	switch message.Type {
	case "approval_pending":
		if message.Approval != nil {
			m.upsertApproval(*message.Approval)
			m.Status = "需要用户授权；按 1/2/3 决定，c 取消。"
		}
	case "approval_resolved", "approval_cancelled":
		id := ""
		if message.Approval != nil {
			id = message.Approval.ID
		}
		if message.Decision != nil {
			id = message.Decision.ApprovalID
		}
		m.removeApproval(id)
	case "questions":
		// A run blocked inside ask_user pushes its pending questions; they
		// arrive live and open the question dialog through the queue.
		m.applyQuestions(message.Questions, false)
	case "todo":
		m.Todos = append([]sessionlog.TaskSnapshot(nil), message.Tasks...)
	case "plan_state":
		if message.PlanState != nil {
			m.Plan = message.PlanState
		}
	case "plan_approval_pending", "plan_approvals":
		for _, ref := range message.PlanApprovals {
			m.upsertPlanApproval(ref)
		}
		if _, ok := m.activePlanApproval(); ok {
			m.Status = "计划等待审批：自动接受、逐次确认或提供反馈。"
		}
	case "plan_approval_resolved":
		m.clearPlanApprovals()
		if message.PlanState != nil {
			m.Plan = message.PlanState
		}
	case "run_started":
		if m.Pending {
			m.ActiveRunID = message.RunID
		}
		m.Status = "正在生成…"
	case "run_event":
		if message.RunEvent == nil {
			return
		}
		if m.Pending && m.ActiveRunID != "" && message.RunEvent.RunID != m.ActiveRunID {
			return
		}
		if m.Pending && message.RunEvent.RunID != "" {
			m.ActiveRunID = message.RunEvent.RunID
		}
		event := sessionlog.Event{SessionID: message.RunEvent.SessionID, Seq: message.Cursor, At: message.RunEvent.At, Type: sessionlog.EventRunEvent, Data: *message.RunEvent}
		if message.Cursor != 0 {
			if message.Cursor > m.LastCursor {
				m.LastCursor = message.Cursor
			}
			found := false
			for _, existing := range m.Events {
				if existing.Seq == message.Cursor && existing.Type == sessionlog.EventRunEvent {
					found = true
					break
				}
			}
			if !found {
				m.Events = append(m.Events, event)
				m.Transcript.SetEvents(m.Events)
			}
		}
		if message.RunEvent.Kind == "text_delta" {
			m.Status = "正在生成…"
		}
		if message.RunEvent.Kind == "error" {
			m.Status = "模型返回错误；已收到的内容已保留。"
		}
	case "resync":
		m.Status = "正在同步遗漏的运行事件…"
	}
}
func (m Model) handleNavigationKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.Navigation = NavigationState{Mode: ChatView}
		m.SearchHits, m.SearchCorrupt = nil, nil
		m.Composer.Focus()
		return m, nil
	case "up", "k":
		if m.Navigation.Cursor > 0 {
			m.Navigation.Cursor--
		}
	case "down", "j":
		limit := len(m.Sessions)
		if m.Navigation.Mode == GoalPickerView {
			limit = len(m.Goals)
		}
		if m.Navigation.Mode == SessionPickerView {
			limit = len(filterSessions(m.Sessions, m.Navigation.Filter))
		}
		if m.Navigation.Mode == SearchResultsView {
			limit = len(m.SearchHits)
		}
		if m.Navigation.Cursor+1 < limit {
			m.Navigation.Cursor++
		}
	case "backspace":
		if m.Navigation.Mode == SessionPickerView && m.Navigation.Filter != "" {
			r := []rune(m.Navigation.Filter)
			m.Navigation.Filter = string(r[:len(r)-1])
			m.Navigation.Cursor = 0
		}
	case "enter":
		if m.Navigation.Mode == SessionPickerView {
			filtered := filterSessions(m.Sessions, m.Navigation.Filter)
			if len(filtered) > 0 {
				m.ActiveSession = filtered[clamp(m.Navigation.Cursor, 0, len(filtered)-1)].ID
				m.Navigation = NavigationState{Mode: ChatView}
				m.Pending = true
				m.Status = "正在加载会话…"
				return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
			}
		}
		if m.Navigation.Mode == GoalPickerView && len(m.Goals) > 0 {
			m.SelectedGoal = clamp(m.Navigation.Cursor, 0, len(m.Goals)-1)
			m.Navigation.Mode = ChatView
		}
		if m.Navigation.Mode == SearchResultsView && len(m.SearchHits) > 0 {
			hit := m.SearchHits[clamp(m.Navigation.Cursor, 0, len(m.SearchHits)-1)]
			m.ActiveSession = hit.Session.ID
			m.Navigation = NavigationState{Mode: ChatView}
			m.SearchHits, m.SearchCorrupt = nil, nil
			m.Pending = true
			m.Status = "正在加载会话…"
			return m, requestCmd(m.Socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: m.Root, SessionID: m.ActiveSession})
		}
	default:
		// In the session picker any other printable key extends the local
		// title/ID filter; it is never submitted anywhere.
		if m.Navigation.Mode == SessionPickerView && k.Type == tea.KeyRunes {
			m.Navigation.Filter += string(k.Runes)
			m.Navigation.Cursor = 0
		}
	}
	return m, nil
}

func (m Model) View() string {
	m.resize()
	if m.Layout.TooSmall {
		return "Stable · 共用对话\n终端窗口较小，请放大窗口。 Ctrl+C 退出。"
	}
	if m.Navigation.Mode == SessionPickerView {
		return "会话\n" + renderSessions(m.Sessions, m.ActiveSession, m.Navigation.Cursor, m.Width, m.Navigation.Filter) + "\n" + renderStatus(m.statusState(), m.ActiveSession, m.Navigation.Mode, m.Width, m.planModeActive())
	}
	if m.Navigation.Mode == GoalPickerView {
		return "目标\n" + renderGoals(m.Goals, m.Navigation.Cursor, m.Width) + "\n" + renderStatus(m.statusState(), m.ActiveSession, m.Navigation.Mode, m.Width, m.planModeActive())
	}
	if m.Navigation.Mode == SearchResultsView {
		return "会话搜索\n" + renderSearchResults(m.SearchHits, m.SearchCorrupt, m.Navigation.Cursor, m.Width) + "\n" + renderStatus(m.statusState(), m.ActiveSession, m.Navigation.Mode, m.Width, m.planModeActive())
	}
	var b strings.Builder
	// The same decision queue that owns the keyboard owns the view: the
	// highest-priority pending dialog renders in place of the chat view.
	switch m.pendingDialog() {
	case DialogApproval:
		return renderApprovalDialog(m.Approvals, m.SelectedApproval, m.Width)
	case DialogQuestion:
		question, _ := m.popupQuestion()
		return renderQuestionDialog(question, m.QuestionCursor, m.QuestionPicked, m.QuestionOther, m.questionOtherText, m.Width)
	case DialogPlan:
		approval, _ := m.activePlanApproval()
		return renderPlanDialog(approval, m.SelectedPlan, m.PlanFeedback, m.Width)
	case DialogReview:
		errorText := ""
		if m.Err != nil {
			errorText = m.Err.Error()
		}
		runActive := m.ActiveRunID != "" || m.Pending
		return renderReview(*m.Review, m.ReviewConfirmed, m.ReviewCursor, m.ReviewSnapshots, m.RewindPick, m.RewindCursor, m.RewindArmed, runActive, m.Status, errorText, m.Width)
	case DialogProposal:
		proposal, _ := m.popupProposal()
		return renderProposalDialog(proposal, m.Width)
	}
	fmt.Fprintf(&b, "%s\n", m.Transcript.View())
	b.WriteString("\n")
	b.WriteString(m.Composer.View())
	if len(m.Candidates) > 0 {
		fmt.Fprintf(&b, "\n%s", (Overlay{Width: m.Width - 4, Height: min(5, max(1, m.Height/4))}).Render(m.Candidates, m.CandidateIndex%len(m.Candidates)))
	}
	if m.Err != nil {
		fmt.Fprintf(&b, "\n错误：%s", m.Err.Error())
	}
	b.WriteString("\n" + renderStatus(m.statusState(), m.ActiveSession, m.Navigation.Mode, m.Width, m.planModeActive()))
	return b.String()
}
func (m Model) statusState() StatusState {
	if m.Err != nil {
		return StatusState{Phase: StatusError, Text: m.Err.Error()}
	}
	if m.Pending {
		return StatusState{Phase: StatusLoading, Text: "等待服务响应"}
	}
	return StatusState{Phase: StatusIdle, Text: m.Status}
}

// planModeActive reports whether the session runs in plan mode. The state is
// restored by session_load and refreshed by plan_state pushes.
func (m Model) planModeActive() bool {
	return m.Plan != nil && m.Plan.Mode == sessionlog.PlanModePlan
}

func Run(socket, root string) error {
	_, err := tea.NewProgram(New(socket, root), tea.WithAltScreen()).Run()
	return err
}
