package tui

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

const tuiTeamPageSize = 20

type teamUIState struct {
	Teams        []teams.Team
	Team         *teams.Team
	TeamMembers  []teams.Member
	TeamTasks    []teams.Task
	TeamMessages []teams.Message
	TeamRequests []teams.Request
}

func registerTeamCommands(host *commandHost, registry *commands.Registry) {
	registry.Register(&commands.Command{Name: "teams", Description: "列出、创建或关闭会话团队", ArgPrompt: "list [limit [after-team-id]] | create 名称 | close ID | coordinator on|off", Kind: commands.KindLocal, Local: func(args string) {
		m := host.model
		fields := strings.Fields(args)
		if m.ActiveSession == "" {
			m.Status = "先选择一个会话。"
			return
		}
		var req conversation.ClientMsg
		switch {
		case (len(fields) >= 1 && len(fields) <= 3) && fields[0] == "list":
			req = conversation.ClientMsg{Op: "team_list", SessionID: m.ActiveSession}
			if len(fields) >= 2 {
				limit, err := strconv.Atoi(fields[1])
				if err != nil || limit < 1 || limit > teams.MaxPageSize {
					m.Status = "用法：/teams list [条数，1-100 [上一页末尾团队ID]]"
					return
				}
				req.Limit = limit
				if len(fields) == 3 {
					if teams.ValidateID(fields[2]) != nil {
						m.Status = "用法：/teams list [条数，1-100 [上一页末尾团队ID]]"
						return
					}
					req.AfterTeamID = fields[2]
				}
			}
		case len(fields) >= 2 && fields[0] == "create":
			name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "create"))
			_, nameErr := teams.NormalizeName(name)
			if strings.ContainsAny(name, " \t\r\n") || nameErr != nil {
				m.Status = "用法：/teams create <单个名称>"
				return
			}
			if m.ActiveRunID == "" {
				m.Status = "创建团队需要活动的 lead run；先发起一次对话，再在运行期间执行 /teams create <名称>。"
				return
			}
			req = conversation.ClientMsg{Op: "team_create", SessionID: m.ActiveSession, RunID: m.ActiveRunID, TeamName: name}
		case len(fields) == 2 && fields[0] == "close" && teams.ValidateID(fields[1]) == nil:
			req = conversation.ClientMsg{Op: "team_close", SessionID: m.ActiveSession, TeamID: fields[1]}
		case len(fields) == 2 && fields[0] == "coordinator" && (fields[1] == "on" || fields[1] == "off"):
			req = conversation.ClientMsg{Op: "team_coordinator", SessionID: m.ActiveSession, CoordinatorOn: fields[1] == "on"}
		default:
			m.Status = "用法：/teams list [条数，1-100 [上一页末尾团队ID]] | /teams create <名称> | /teams close <ID> | /teams coordinator on|off"
			return
		}
		sendTeamRequest(host, req, "正在读取团队…")
	}})

	registry.Register(&commands.Command{Name: "team", Description: "查看团队、任务、消息和请求", ArgPrompt: "ID get|members [limit [after-member-id]]|tasks list [limit [after-task-id]]|messages|requests [list [limit [after-request-id]]]|send|respond|shutdown", Kind: commands.KindLocal, Local: func(args string) {
		m := host.model
		fields := strings.Fields(args)
		if m.ActiveSession == "" {
			m.Status = "先选择一个会话。"
			return
		}
		if len(fields) < 2 || teams.ValidateID(fields[0]) != nil {
			m.Status = teamUsage()
			return
		}
		teamID, action := fields[0], fields[1]
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]+" "+action))
		base := conversation.ClientMsg{SessionID: m.ActiveSession, TeamID: teamID}
		var req conversation.ClientMsg
		switch action {
		case "get":
			if rest != "" {
				m.Status = "用法：/team <ID> get"
				return
			}
			base.Op = "team_get"
			req = base
		case "members":
			limit := 0
			afterMemberID := ""
			if rest != "" {
				fields := strings.Fields(rest)
				if len(fields) < 1 || len(fields) > 2 {
					m.Status = "用法：/team <ID> members [条数，1-100 [上一页末尾成员ID]]"
					return
				}
				parsed, err := strconv.Atoi(fields[0])
				if err != nil || parsed < 1 || parsed > teams.MaxPageSize {
					m.Status = "用法：/team <ID> members [条数，1-100 [上一页末尾成员ID]]"
					return
				}
				limit = parsed
				if len(fields) == 2 {
					if teams.ValidateID(fields[1]) != nil {
						m.Status = "用法：/team <ID> members [条数，1-100 [上一页末尾成员ID]]"
						return
					}
					afterMemberID = fields[1]
				}
			}
			if err := m.showTeamMembers(teamID, limit, afterMemberID); err != nil {
				m.Status = err.Error()
			}
			return
		case "tasks":
			req, m.Status = teamTaskRequest(base, rest)
			if req.Op == "" {
				return
			}
		case "messages":
			var err error
			req, err = teamMessagesRequest(base, rest)
			if err != nil {
				m.Status = err.Error()
				return
			}
		case "requests":
			fields := strings.Fields(rest)
			if len(fields) > 3 || len(fields) > 0 && fields[0] != "list" {
				m.Status = "用法：/team <ID> requests [list [条数，1-100 [上一页末尾请求ID]]]"
				return
			}
			base.Op = "team_request_list"
			if len(fields) == 2 || len(fields) == 3 {
				limitText := fields[1]
				limit, err := strconv.Atoi(limitText)
				if err != nil || limit < 1 || limit > teams.MaxPageSize {
					m.Status = "用法：/team <ID> requests [list [条数，1-100 [上一页末尾请求ID]]]"
					return
				}
				base.Limit = limit
				if len(fields) == 3 {
					if teams.ValidateID(fields[2]) != nil {
						m.Status = "用法：/team <ID> requests [list [条数，1-100 [上一页末尾请求ID]]]"
						return
					}
					base.AfterTeamRequestID = fields[2]
				}
			}
			req = base
		case "send":
			req, m.Status = teamSendRequest(base, rest)
			if req.Op == "" {
				return
			}
		case "respond":
			req, m.Status = teamRespondRequest(base, rest)
			if req.Op == "" {
				return
			}
		case "shutdown":
			if len(strings.Fields(rest)) != 1 || teams.ValidateID(strings.TrimSpace(rest)) != nil {
				m.Status = "用法：/team <ID> shutdown <成员ID>"
				return
			}
			base.Op, base.TeamMemberID = "team_shutdown_request", strings.TrimSpace(rest)
			req = base
		case "stop":
			if len(strings.Fields(rest)) != 1 || teams.ValidateID(strings.TrimSpace(rest)) != nil {
				m.Status = "用法：/team <ID> stop <成员ID>"
				return
			}
			base.Op, base.TeamMemberID = "team_member_stop", strings.TrimSpace(rest)
			req = base
		case "spawn":
			spawn, err := teamMemberSpawnRequest(base, rest, m.ActiveRunID)
			if err != nil {
				m.Status = err.Error()
				return
			}
			req = spawn
		case "resume":
			fields := strings.Fields(rest)
			acceptRoleChange := len(fields) == 2 && fields[1] == "--accept-role-change"
			if (len(fields) != 1 && !acceptRoleChange) || teams.ValidateID(fields[0]) != nil || m.ActiveRunID == "" {
				m.Status = "用法：/team <ID> resume <成员ID> [--accept-role-change]（需要活动的 lead run）"
				return
			}
			base.Op, base.RunID, base.TeamMemberID = "team_member_resume", m.ActiveRunID, fields[0]
			base.TeamAcceptRoleChange = acceptRoleChange
			req = base
		default:
			m.Status = teamUsage()
			return
		}
		sendTeamRequest(host, req, "正在读取团队状态…")
	}})
}

func teamUsage() string {
	return "用法：/team <ID> get | members | spawn <成员名> <角色> <任务> [--plan] | resume <成员ID> [--accept-role-change] | stop <成员ID> | tasks [list [条数] | get ID | create 标题 | update ID REVISION status 状态] | messages [游标 [条数]] | requests | send <成员ID|all> <消息> | respond <请求ID> <REVISION> approve|reject|defer [反馈] | shutdown <成员ID>"
}

func teamMemberSpawnRequest(base conversation.ClientMsg, args, activeRunID string) (conversation.ClientMsg, error) {
	fields := strings.Fields(args)
	if activeRunID == "" || len(fields) < 3 {
		return conversation.ClientMsg{}, fmt.Errorf("用法：/team <ID> spawn <成员名> <角色> <任务> [--plan]（需要活动的 lead run）")
	}
	name, nameErr := teams.NormalizeMemberName(fields[0])
	role, roleErr := teams.NormalizeName(fields[1])
	instruction := restAfterFields(args, 2)
	planRequired := false
	if strings.HasSuffix(instruction, " --plan") || instruction == "--plan" {
		instruction = strings.TrimSpace(strings.TrimSuffix(instruction, "--plan"))
		planRequired = true
	}
	if nameErr != nil || roleErr != nil || teams.ValidateText(instruction, teams.MaxInputBytes, true) != nil {
		return conversation.ClientMsg{}, fmt.Errorf("成员名/角色无效或任务超过 %d 字节。", teams.MaxInputBytes)
	}
	base.Op, base.RunID, base.TeamMemberName, base.AgentName, base.Text, base.TeamPlanRequired = "team_member_spawn", activeRunID, name, role, instruction, planRequired
	return base, nil
}

func sendTeamRequest(host *commandHost, req conversation.ClientMsg, status string) {
	m := host.model
	req.SessionID = m.ActiveSession
	m.Err = nil
	m.Status = status
	m.Composer.SetValue("")
	m.recordHistory(host.raw)
	host.send(requestCmd(m.Socket, req))
}

func teamTaskRequest(base conversation.ClientMsg, args string) (conversation.ClientMsg, string) {
	fields := strings.Fields(args)
	base.Op = "team_task_list"
	if len(fields) == 0 {
		return base, "正在读取团队任务…"
	}
	if fields[0] == "list" {
		if len(fields) == 1 {
			return base, "正在读取团队任务…"
		}
		if len(fields) == 2 || len(fields) == 3 {
			limit, err := strconv.Atoi(fields[1])
			if err == nil && limit >= 1 && limit <= teams.MaxPageSize {
				base.Limit = limit
				if len(fields) == 3 {
					if teams.ValidateID(fields[2]) != nil {
						return conversation.ClientMsg{}, "用法：/team <ID> tasks list [条数，1-100 [上一页末尾任务ID]]"
					}
					base.AfterTaskID = fields[2]
				}
				return base, "正在读取团队任务…"
			}
		}
		return conversation.ClientMsg{}, "用法：/team <ID> tasks list [条数，1-100 [上一页末尾任务ID]]"
	}
	switch {
	case len(fields) == 2 && fields[0] == "get" && teams.ValidateID(fields[1]) == nil:
		base.Op, base.TaskID = "team_task_get", fields[1]
	case len(fields) >= 2 && fields[0] == "create":
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "create"))
		if err := teams.ValidateText(title, teams.MaxTaskTitleBytes, true); err != nil {
			return conversation.ClientMsg{}, "用法：/team <ID> tasks create <标题（最多 256 字节）>"
		}
		base.Op, base.TaskTitle = "team_task_create", &title
	case len(fields) == 5 && fields[0] == "update" && teams.ValidateID(fields[1]) == nil && fields[3] == "status":
		revision, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil || revision == 0 || (fields[4] != string(teams.TaskPending) && fields[4] != string(teams.TaskInProgress) && fields[4] != string(teams.TaskCompleted)) {
			return conversation.ClientMsg{}, "用法：/team <ID> tasks update <任务ID> <revision> status pending|in_progress|completed"
		}
		status := fields[4]
		base.Op, base.TaskID, base.ExpectedRevision, base.TaskStatus = "team_task_update", fields[1], revision, &status
	default:
		return conversation.ClientMsg{}, "用法：/team <ID> tasks [list [条数，1-100 [上一页末尾任务ID]] | get <任务ID> | create <标题> | update <任务ID> <revision> status <状态>]"
	}
	return base, "正在读取团队任务…"
}

func teamMessagesRequest(base conversation.ClientMsg, args string) (conversation.ClientMsg, error) {
	base.Op, base.Limit = "team_messages", tuiTeamPageSize
	fields := strings.Fields(args)
	if len(fields) == 0 || (len(fields) == 1 && fields[0] == "list") {
		return base, nil
	}
	if len(fields) >= 2 && fields[0] == "after" {
		fields = fields[1:]
	}
	if len(fields) < 1 || len(fields) > 2 {
		return conversation.ClientMsg{}, fmt.Errorf("用法：/team <ID> messages [after <序号>] [条数（1-%d）]", teams.MaxPageSize)
	}
	after, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return conversation.ClientMsg{}, fmt.Errorf("消息游标必须是非负整数：%w", err)
	}
	base.AfterSeq = after
	if len(fields) == 2 {
		limit, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil || limit < 1 || limit > teams.MaxPageSize {
			return conversation.ClientMsg{}, fmt.Errorf("用法：/team <ID> messages [after <序号>] [条数（1-%d）]", teams.MaxPageSize)
		}
		base.Limit = limit
	}
	return base, nil
}

func teamSendRequest(base conversation.ClientMsg, args string) (conversation.ClientMsg, string) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return conversation.ClientMsg{}, "用法：/team <ID> send <成员ID|all> <消息>"
	}
	recipient := fields[0]
	if recipient != "all" && recipient != teams.Lead && teams.ValidateID(recipient) != nil {
		return conversation.ClientMsg{}, "收件人应为 lead、成员 ID 或 all。"
	}
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), recipient))
	if strings.TrimSpace(body) == "" || teams.ValidateText(body, teams.MaxMessageBytes, true) != nil {
		return conversation.ClientMsg{}, fmt.Sprintf("消息不能为空或超过 %d 字节。用法：/team <ID> send <成员ID|all> <消息>", teams.MaxMessageBytes)
	}
	token, err := sessionlog.NewID()
	if err != nil {
		return conversation.ClientMsg{}, "无法生成消息幂等 ID：" + err.Error()
	}
	base.Op, base.Text, base.TeamToken = "team_send", body, token
	if recipient == "all" {
		base.TeamBroadcast = true
	} else {
		base.TeamRecipient = recipient
	}
	return base, "正在发送团队消息…"
}

func teamRespondRequest(base conversation.ClientMsg, args string) (conversation.ClientMsg, string) {
	fields := strings.Fields(args)
	if len(fields) < 3 || teams.ValidateID(fields[0]) != nil {
		return conversation.ClientMsg{}, "用法：/team <ID> respond <请求ID> <revision> approve|reject|defer [反馈]"
	}
	revision, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || revision == 0 {
		return conversation.ClientMsg{}, "用法：/team <ID> respond <请求ID> <revision> approve|reject|defer [反馈]；revision 必须是正整数。"
	}
	decision := ""
	switch fields[2] {
	case "approve":
		decision = string(teams.RequestApproved)
	case "reject":
		decision = string(teams.RequestRejected)
	case "defer":
		decision = string(teams.RequestDeferred)
	default:
		return conversation.ClientMsg{}, "决定必须是 approve、reject 或 defer。"
	}
	feedback := restAfterFields(args, 3)
	if teams.ValidateText(feedback, teams.MaxFeedbackBytes, false) != nil {
		return conversation.ClientMsg{}, fmt.Sprintf("反馈不能超过 %d 字节。", teams.MaxFeedbackBytes)
	}
	base.Op, base.TeamRequestID, base.ExpectedRevision, base.TeamDecision, base.TeamFeedback = "team_request_respond", fields[0], revision, decision, feedback
	return base, "正在记录团队请求响应…"
}

func (m Model) handleTeamResult(r resultMsg) (tea.Model, tea.Cmd) {
	for _, msg := range r.msgs {
		m.captureTeamMessage(msg)
	}
	if r.err != nil {
		m.Err = r.err
		m.Status = "团队请求失败：" + r.err.Error()
		if len(r.msgs) > 0 {
			m.appendTeamNote(formatTeamMessages(r.msgs))
		}
		return m, nil
	}
	m.Err = nil
	if len(r.msgs) == 0 {
		m.Status = "团队请求已完成。"
		return m, nil
	}
	text := formatTeamMessages(r.msgs)
	if text == "" {
		m.Status = "团队请求已完成。"
		return m, nil
	}
	m.appendTeamNote(text)
	m.Status = teamResultStatus(r.op, r.msgs)
	return m, nil
}

func restAfterFields(input string, count int) string {
	remaining := input
	for range count {
		remaining = strings.TrimLeft(remaining, " \t\r\n")
		end := strings.IndexAny(remaining, " \t\r\n")
		if end < 0 {
			return ""
		}
		remaining = remaining[end:]
	}
	return strings.TrimSpace(remaining)
}

func (m *Model) captureTeamMessage(msg conversation.ServerMsg) {
	switch msg.Type {
	case "team_list":
		m.Teams = append([]teams.Team(nil), msg.Teams...)
	case "team_task_list":
		m.TeamTasks = append([]teams.Task(nil), msg.TeamTasks...)
	case "team_messages":
		m.TeamMessages = append([]teams.Message(nil), msg.TeamMessages...)
	case "team_request_list":
		m.TeamRequests = append([]teams.Request(nil), msg.TeamRequests...)
	}
	if msg.Team != nil {
		team := *msg.Team
		m.Team = &team
	}
	if msg.TeamTask != nil {
		m.TeamTasks = upsertTeamTask(m.TeamTasks, *msg.TeamTask)
	}
	if msg.TeamMessage != nil {
		m.TeamMessages = upsertTeamMessage(m.TeamMessages, *msg.TeamMessage)
	}
	if msg.TeamRequest != nil {
		m.TeamRequests = upsertTeamRequest(m.TeamRequests, *msg.TeamRequest)
	}
	if msg.TeamMember != nil {
		m.TeamMembers = upsertTeamMember(m.TeamMembers, *msg.TeamMember)
	}
}

func (m *Model) restoreTeamState() {
	m.teamUIState = teamUIState{}
	projection, err := sessionlog.ProjectTeams(sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: m.ActiveSession}, Events: m.Events})
	if err != nil {
		return
	}
	for _, team := range projection.Teams {
		m.Teams = append(m.Teams, team)
	}
	for _, member := range projection.Members {
		m.TeamMembers = append(m.TeamMembers, member)
	}
	for _, task := range projection.Tasks {
		m.TeamTasks = append(m.TeamTasks, task)
	}
	for _, message := range projection.Messages {
		m.TeamMessages = append(m.TeamMessages, message)
	}
	for _, request := range projection.Requests {
		m.TeamRequests = append(m.TeamRequests, request)
	}
	sort.Slice(m.Teams, func(i, j int) bool {
		if m.Teams[i].Name == m.Teams[j].Name {
			return m.Teams[i].ID < m.Teams[j].ID
		}
		return m.Teams[i].Name < m.Teams[j].Name
	})
	sort.Slice(m.TeamMembers, func(i, j int) bool {
		if m.TeamMembers[i].TeamID == m.TeamMembers[j].TeamID {
			return m.TeamMembers[i].Name < m.TeamMembers[j].Name
		}
		return m.TeamMembers[i].TeamID < m.TeamMembers[j].TeamID
	})
	sort.Slice(m.TeamTasks, func(i, j int) bool {
		if m.TeamTasks[i].TeamID == m.TeamTasks[j].TeamID {
			return m.TeamTasks[i].ID < m.TeamTasks[j].ID
		}
		return m.TeamTasks[i].TeamID < m.TeamTasks[j].TeamID
	})
	sort.Slice(m.TeamMessages, func(i, j int) bool {
		if m.TeamMessages[i].TeamID == m.TeamMessages[j].TeamID {
			return m.TeamMessages[i].Seq < m.TeamMessages[j].Seq
		}
		return m.TeamMessages[i].TeamID < m.TeamMessages[j].TeamID
	})
	sort.Slice(m.TeamRequests, func(i, j int) bool {
		if m.TeamRequests[i].TeamID == m.TeamRequests[j].TeamID {
			return m.TeamRequests[i].ID < m.TeamRequests[j].ID
		}
		return m.TeamRequests[i].TeamID < m.TeamRequests[j].TeamID
	})
}

func upsertTeamTask(items []teams.Task, item teams.Task) []teams.Task {
	for i := range items {
		if items[i].ID == item.ID {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}
func upsertTeamMember(items []teams.Member, item teams.Member) []teams.Member {
	for i := range items {
		if items[i].ID == item.ID {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}
func upsertTeamMessage(items []teams.Message, item teams.Message) []teams.Message {
	for i := range items {
		if items[i].ID == item.ID {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}
func upsertTeamRequest(items []teams.Request, item teams.Request) []teams.Request {
	for i := range items {
		if items[i].ID == item.ID {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func (m *Model) showTeamMembers(teamID string, limit int, afterMemberID string) error {
	projection, err := sessionlog.ProjectTeams(sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: m.ActiveSession}, Events: m.Events})
	if err != nil {
		return fmt.Errorf("无法读取会话团队投影：%w", err)
	}
	members := make([]teams.Member, 0)
	for _, member := range projection.Members {
		if member.TeamID == teamID {
			members = append(members, member)
		}
	}
	if _, ok := projection.Teams[teamID]; !ok {
		return errors.New("当前会话中找不到该团队。")
	}
	page, err := teams.MembersPageAfter(members, afterMemberID, limit)
	if err != nil {
		return err
	}
	m.TeamMembers = page
	m.appendTeamNote(formatTeamMembers(teamID, m.TeamMembers))
	m.Status = fmt.Sprintf("团队成员：%d/%d", len(m.TeamMembers), len(members))
	return nil
}

func (m *Model) appendTeamNote(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: text, Kind: "text"}})
	m.Transcript.SetEvents(m.Events)
}

func formatTeamMessages(messages []conversation.ServerMsg) string {
	var b strings.Builder
	for _, msg := range messages {
		switch msg.Type {
		case "team_member_spawn", "team_member_resume":
			if msg.TeamMember != nil {
				fmt.Fprintf(&b, "成员 %s · %s · %s · %s\n", msg.TeamMember.Name, msg.TeamMember.ID, msg.TeamMember.AgentName, msg.TeamMember.Status)
			}
		case "team_list":
			fmt.Fprintf(&b, "团队（%d）\n", len(msg.Teams))
			for _, team := range msg.Teams {
				fmt.Fprintf(&b, "- %s · %s · %s（revision %d）\n", team.Name, team.ID, team.Status, team.Revision)
			}
		case "team_create", "team_get", "team_close":
			if msg.Team != nil {
				fmt.Fprintf(&b, "团队 %s · %s · %s（revision %d）\n", msg.Team.Name, msg.Team.ID, msg.Team.Status, msg.Team.Revision)
			}
		case "team_task_list":
			fmt.Fprintf(&b, "团队任务（%d）\n", len(msg.TeamTasks))
			for _, task := range msg.TeamTasks {
				fmt.Fprintf(&b, "- %s · %s · %s（revision %d）", task.ID, task.Title, task.Status, task.Revision)
				if len(task.BlockedBy) > 0 {
					fmt.Fprintf(&b, "；依赖 %s", strings.Join(task.BlockedBy, ", "))
				}
				b.WriteByte('\n')
			}
		case "team_task_get", "team_task_create", "team_task_update":
			if msg.TeamTask != nil {
				fmt.Fprintf(&b, "任务 %s · %s · %s（revision %d）\n", msg.TeamTask.ID, msg.TeamTask.Title, msg.TeamTask.Status, msg.TeamTask.Revision)
			}
		case "team_messages":
			fmt.Fprintf(&b, "团队消息（%d）\n", len(msg.TeamMessages))
			for _, item := range msg.TeamMessages {
				fmt.Fprintf(&b, "- #%d %s → %s：%s\n", item.Seq, item.SenderID, strings.Join(item.Recipients, ","), item.Body)
			}
		case "team_send":
			if msg.TeamMessage != nil {
				fmt.Fprintf(&b, "消息已发送：#%d → %s\n", msg.TeamMessage.Seq, strings.Join(msg.TeamMessage.Recipients, ","))
			}
		case "team_request_list":
			fmt.Fprintf(&b, "团队请求（%d）\n", len(msg.TeamRequests))
			for _, request := range msg.TeamRequests {
				fmt.Fprintf(&b, "- %s · %s · %s · revision %d · 到期 %s", request.ID, request.Type, request.Status, request.Revision, request.ExpiresAt.Format("2006-01-02 15:04"))
				if request.Body != "" {
					fmt.Fprintf(&b, "\n  %s", request.Body)
				}
				b.WriteByte('\n')
			}
		case "team_request_respond", "team_shutdown_request":
			if msg.TeamRequest != nil {
				fmt.Fprintf(&b, "团队请求 %s · %s · revision %d\n", msg.TeamRequest.ID, msg.TeamRequest.Status, msg.TeamRequest.Revision)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTeamMembers(teamID string, members []teams.Member) string {
	var b strings.Builder
	fmt.Fprintf(&b, "团队成员 %s（%d）\n", teamID, len(members))
	for _, member := range members {
		fmt.Fprintf(&b, "- %s · %s · %s · %s", member.Name, member.ID, member.AgentName, member.Status)
		if member.Summary != "" {
			fmt.Fprintf(&b, "\n  %s", member.Summary)
		}
		if member.Error != "" {
			fmt.Fprintf(&b, "\n  错误：%s", member.Error)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func teamResultStatus(op string, messages []conversation.ServerMsg) string {
	switch op {
	case "team_coordinator":
		if len(messages) > 0 && messages[len(messages)-1].CoordinatorOn {
			return "团队协调器模式已设为下一次运行启用。"
		}
		return "团队协调器模式已关闭。"
	case "team_list":
		return fmt.Sprintf("团队列表已更新（%d）。", len(firstTeamList(messages)))
	case "team_task_list":
		return fmt.Sprintf("团队任务已更新（%d）。", len(firstTeamTasks(messages)))
	case "team_messages":
		return fmt.Sprintf("团队消息已更新（%d）。", len(firstTeamMessages(messages)))
	case "team_request_list":
		return fmt.Sprintf("团队请求已更新（%d）。", len(firstTeamRequests(messages)))
	case "team_send":
		return "团队消息已发送。"
	case "team_request_respond":
		return "团队请求响应已记录。"
	case "team_shutdown_request":
		return "成员停止请求已提交。"
	case "team_close":
		return "团队关闭状态已更新。"
	case "team_create":
		return "团队已创建。"
	case "team_member_spawn":
		return "团队成员已加入共享池。"
	case "team_member_resume":
		return "团队成员已安排下一轮。"
	case "team_member_stop":
		return "已请求取消成员当前 turn，等待 child 实际退出。"
	default:
		return "团队状态已更新。"
	}
}

func firstTeamList(messages []conversation.ServerMsg) []teams.Team {
	for _, msg := range messages {
		if msg.Teams != nil {
			return msg.Teams
		}
	}
	return nil
}
func firstTeamTasks(messages []conversation.ServerMsg) []teams.Task {
	for _, msg := range messages {
		if msg.TeamTasks != nil {
			return msg.TeamTasks
		}
	}
	return nil
}
func firstTeamMessages(messages []conversation.ServerMsg) []teams.Message {
	for _, msg := range messages {
		if msg.TeamMessages != nil {
			return msg.TeamMessages
		}
	}
	return nil
}
func firstTeamRequests(messages []conversation.ServerMsg) []teams.Request {
	for _, msg := range messages {
		if msg.TeamRequests != nil {
			return msg.TeamRequests
		}
	}
	return nil
}
