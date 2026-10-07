package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
)

// The session task stream owns its cursor and never owns the parent run.
type agentTaskState struct {
	agentSession    string
	agentCursor     uint64
	agentListCursor uint64
	agentStream     *conversation.StreamClient
	agentConnecting bool
	agentGeneration uint64
	agentLookups    map[string]bool
}

type agentStreamStartedMsg struct {
	client     *conversation.StreamClient
	sessionID  string
	generation uint64
	err        error
}
type agentStreamMsg struct {
	client     *conversation.StreamClient
	sessionID  string
	generation uint64
	message    conversation.ServerMsg
	err        error
}
type agentReconnectMsg struct {
	sessionID  string
	generation uint64
}

func registerAgentCommands(host *commandHost, registry *commands.Registry) {
	register := func(name, description, prompt string, fn func(string)) {
		registry.Register(&commands.Command{Name: name, Description: description, ArgPrompt: prompt, Kind: commands.KindLocal, Local: fn})
	}
	send := func(req conversation.ClientMsg) {
		m := host.model
		if m.ActiveSession == "" {
			m.Status = "先选择一个会话。"
			return
		}
		req.SessionID = m.ActiveSession
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		m.Status = "正在查询只读 agent 任务…"
		host.send(requestCmd(m.Socket, req))
	}
	register("agents", "列出只读 agent 角色", "reload", func(args string) {
		op := "agent_list"
		switch strings.TrimSpace(args) {
		case "":
		case "reload":
			op = "agent_reload"
		default:
			host.model.Status = "用法：/agents [reload]"
			return
		}
		send(conversation.ClientMsg{Op: op})
	})
	register("agent", "启动独立只读后台任务", "角色 任务", func(args string) {
		fields := strings.Fields(args)
		if len(fields) < 2 {
			host.model.Status = "用法：/agent <角色> <任务>"
			return
		}
		name := fields[0]
		instruction := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), name))
		send(conversation.ClientMsg{Op: "agent_task_start", AgentName: name, Text: instruction, Background: true})
	})
	register("tasks", "查看或取消本会话 agent 任务", "[next | get ID | stop ID]", func(args string) {
		fields := strings.Fields(args)
		req := conversation.ClientMsg{Op: "agent_task_list", Limit: 20}
		switch {
		case len(fields) == 0:
			host.model.agentListCursor = 0
		case len(fields) == 1 && fields[0] == "next":
			req.AfterSeq = host.model.agentListCursor
		case len(fields) == 2 && fields[0] == "get":
			req.Op, req.TaskID = "agent_task_get", fields[1]
		case len(fields) == 2 && fields[0] == "stop":
			req.Op, req.TaskID = "agent_task_cancel", fields[1]
		default:
			host.model.Status = "用法：/tasks [next | get <ID> | stop <ID>]"
			return
		}
		send(req)
	})
}

func agentRequestCmd(socket string, req conversation.ClientMsg, operation string) tea.Cmd {
	return func() tea.Msg {
		result := requestCmd(socket, req)().(resultMsg)
		result.op = operation
		return result
	}
}

func (m *Model) closeAgentStream() {
	if m.agentStream != nil {
		_ = m.agentStream.Close()
	}
	m.agentStream = nil
	m.agentConnecting = false
	m.agentGeneration++
}

func (m *Model) prepareAgentSession() {
	if m.agentSession == m.ActiveSession {
		return
	}
	m.closeAgentStream()
	m.agentSession = m.ActiveSession
	m.agentCursor, m.agentListCursor = 0, 0
	m.AgentTasks = nil
	m.agentLookups = map[string]bool{}
}

func (m *Model) ensureAgentStream() tea.Cmd {
	m.prepareAgentSession()
	if m.ActiveSession == "" || m.agentStream != nil || m.agentConnecting {
		return nil
	}
	m.agentConnecting = true
	socket, sessionID, cursor, generation := m.Socket, m.agentSession, m.agentCursor, m.agentGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := conversation.SubscribeAgentTasks(ctx, socket, sessionID, cursor)
		return agentStreamStartedMsg{client: client, sessionID: sessionID, generation: generation, err: err}
	}
}

func receiveAgentTaskCmd(client *conversation.StreamClient, sessionID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		message, err := client.Receive()
		return agentStreamMsg{client: client, sessionID: sessionID, generation: generation, message: message, err: err}
	}
}

func (m *Model) restoreAgentTasks() tea.Cmd {
	m.prepareAgentSession()
	// Rebuild trusted task state from durable facts, including tasks beyond
	// the first list page, so reconnection needs no per-task subscription.
	if records, err := sessionlog.AgentTasks(sessionlog.Transcript{Session: sessionlog.SessionInfo{ID: m.ActiveSession}, Events: m.Events}); err == nil {
		for _, record := range records {
			status := agent.DelegationStatus(record.Delegation.Status)
			if status == "" {
				status = agent.DelegationQueued
				switch record.RunStatus {
				case string(agent.RunFailed):
					status = agent.DelegationFailed
				case string(agent.RunInterrupted):
					status = agent.DelegationInterrupted
				case string(agent.RunCancelled):
					status = agent.DelegationCanceled
				case string(agent.RunCompleted):
					status = agent.DelegationSucceeded
				}
			}
			reason := record.Delegation.Error
			if reason == "" && record.RunStatus != "completed" {
				reason = record.TerminalReason
			}
			m.applyAgentTask(agent.AgentTaskSnapshot{ID: record.Started.AgentTaskID, RunID: record.Started.RunID, OriginRunID: record.Started.OriginRunID, SessionID: m.ActiveSession, AgentName: record.Started.AgentName, Name: record.Started.AgentName, Status: status, Stage: record.Delegation.Stage, Summary: record.Delegation.Summary, Error: reason, Cursor: record.LastSeq}, false)
		}
	}
	for _, event := range m.Events {
		if event.Seq > m.agentCursor {
			m.agentCursor = event.Seq
		}
	}
	return tea.Batch(
		agentRequestCmd(m.Socket, conversation.ClientMsg{Op: "agent_task_list", SessionID: m.ActiveSession, Limit: 20}, "agent_task_restore"),
		m.ensureAgentStream(),
	)
}

func (m Model) handleAgentStreamStarted(msg agentStreamStartedMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.ActiveSession || msg.generation != m.agentGeneration {
		if msg.client != nil {
			_ = msg.client.Close()
		}
		return m, nil
	}
	m.agentConnecting = false
	if msg.err != nil {
		m.Status = "后台任务连接中断，正在重连…"
		return m, m.agentRetryCmd()
	}
	m.agentStream = msg.client
	return m, receiveAgentTaskCmd(msg.client, msg.sessionID, msg.generation)
}

func (m Model) agentRetryCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return agentReconnectMsg{sessionID: m.agentSession, generation: m.agentGeneration}
	})
}

func (m Model) handleAgentReconnect(msg agentReconnectMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.ActiveSession || msg.generation != m.agentGeneration {
		return m, nil
	}
	command := m.ensureAgentStream()
	return m, tea.Batch(command, agentRequestCmd(m.Socket, conversation.ClientMsg{Op: "agent_task_list", SessionID: m.ActiveSession, Limit: 20}, "agent_task_restore"))
}

func (m Model) handleAgentStreamMessage(msg agentStreamMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.ActiveSession || msg.generation != m.agentGeneration || msg.client != m.agentStream {
		return m, nil
	}
	if msg.err != nil || msg.message.Type == "error" {
		if msg.client != nil {
			_ = msg.client.Close()
		}
		m.agentStream = nil
		m.Status = "后台任务连接中断，正在重连…"
		return m, m.agentRetryCmd()
	}
	message := msg.message
	var lookup tea.Cmd
	if message.Type == "agent_task_update" && message.AgentTask != nil {
		m.applyAgentTask(*message.AgentTask, true)
	}
	if message.Type == "run_event" && message.RunEvent != nil && message.RunEvent.SessionID == m.ActiveSession && message.RunEvent.Kind == string(agent.EventDelegation) {
		var update agent.DelegationEvent
		if decodeEventData(message.RunEvent.Payload, &update) == nil {
			known := false
			for _, task := range m.AgentTasks {
				if task.RunID != message.RunEvent.RunID || task.ID != update.TaskID {
					continue
				}
				known = true
				task.Status, task.Stage, task.Summary, task.Error, task.Cursor = update.Status, update.Stage, update.Summary, update.Error, message.Cursor
				m.applyAgentTask(task, true)
				break
			}
			// A task started through a parent tool may not yet be in the list.
			// Resolve its task ID through the authorized get operation before
			// treating it as a background agent rather than another delegate.
			if !known && update.TaskID != "" {
				if m.agentLookups == nil {
					m.agentLookups = map[string]bool{}
				}
				if !m.agentLookups[update.TaskID] && len(m.agentLookups) < 100 {
					m.agentLookups[update.TaskID] = true
					lookup = agentRequestCmd(m.Socket, conversation.ClientMsg{Op: "agent_task_get", SessionID: m.ActiveSession, TaskID: update.TaskID}, "agent_task_refresh")
				}
			}
		}
	}
	if message.Cursor > m.agentCursor {
		m.agentCursor = message.Cursor
	}
	if message.Type == "resync" {
		m.agentCursor = 0
		_ = msg.client.Close()
		m.agentStream = nil
		command := m.ensureAgentStream()
		return m, command
	}
	return m, tea.Batch(receiveAgentTaskCmd(msg.client, msg.sessionID, msg.generation), lookup)
}

func (m Model) handleAgentResult(result resultMsg) (tea.Model, tea.Cmd) {
	if result.sessionID != "" && result.sessionID != m.ActiveSession {
		return m, nil
	}
	m.prepareAgentSession()
	if result.op == "agent_task_refresh" {
		delete(m.agentLookups, result.taskID)
	}
	quiet := result.op == "agent_task_restore" || result.op == "agent_task_refresh"
	if result.err != nil {
		if !quiet {
			m.Err = result.err
			m.Status = "Agent 请求失败：" + result.err.Error()
		}
		return m, nil
	}
	if !quiet {
		m.Err = nil
	}
	for _, message := range result.msgs {
		if message.Agents != nil {
			m.appendAgentNote(renderAgentCatalog(*message.Agents))
			if result.op == "agent_reload" {
				m.Status = fmt.Sprintf("Agent 角色已重载：%d 个，跳过 %d 项。", len(message.Agents.Definitions), len(message.Agents.Rejections))
			}
		}
		if message.Type == "agent_task_list" {
			for _, task := range message.AgentTasks {
				m.applyAgentTask(task, false)
				if !quiet && task.Cursor > m.agentListCursor {
					m.agentListCursor = task.Cursor
				}
			}
			if !quiet {
				m.appendAgentNote(renderAgentTaskList(message.AgentTasks))
			}
		}
		if message.AgentTask != nil {
			task := *message.AgentTask
			m.applyAgentTask(task, quiet && result.op == "agent_task_refresh")
			for _, current := range m.AgentTasks {
				if current.ID == task.ID {
					task = current
					break
				}
			}
			if !quiet {
				m.appendAgentNote(renderAgentTask(task))
				if result.op == "agent_task_cancel" && !task.Status.IsTerminal() {
					m.Status = "取消请求已发送，等待任务实际退出。"
					m.appendAgentNote(m.Status)
				} else {
					m.Status = fmt.Sprintf("Agent 任务 %s：%s", task.ID, task.Status)
				}
			}
		}
	}
	command := m.ensureAgentStream()
	return m, command
}

func (m *Model) appendAgentNote(text string) {
	m.Events = append(m.Events, sessionlog.Event{SessionID: m.ActiveSession, Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: text, Kind: "text"}})
	m.Transcript.SetEvents(m.Events)
}

func (m *Model) applyAgentTask(task agent.AgentTaskSnapshot, visible bool) {
	if task.ID == "" || task.RunID == "" || task.SessionID != m.ActiveSession {
		return
	}
	for i, previous := range m.AgentTasks {
		if previous.ID != task.ID {
			continue
		}
		if task.Cursor <= previous.Cursor || previous.Status.IsTerminal() && !task.Status.IsTerminal() {
			return
		}
		m.AgentTasks[i] = task
		if visible && !(previous.Status.IsTerminal() && previous.Status == task.Status && previous.Summary == task.Summary && previous.Error == task.Error) {
			m.appendAgentNote(renderAgentTask(task))
		}
		return
	}
	if len(m.AgentTasks) >= 100 {
		sort.SliceStable(m.AgentTasks, func(i, j int) bool { return m.AgentTasks[i].Cursor < m.AgentTasks[j].Cursor })
		index := -1
		for i, old := range m.AgentTasks {
			if old.Status.IsTerminal() {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		m.AgentTasks = append(m.AgentTasks[:index], m.AgentTasks[index+1:]...)
	}
	m.AgentTasks = append(m.AgentTasks, task)
	if visible {
		m.appendAgentNote(renderAgentTask(task))
	}
}

func renderAgentCatalog(snapshot agentcatalog.Snapshot) string {
	var out strings.Builder
	out.WriteString("只读 Agent 角色：")
	for _, role := range snapshot.Definitions {
		fmt.Fprintf(&out, "\n- %s · %s（来源：%s，模型：%s，最多 %d 轮）\n  工具：%s", role.Name, role.Description, role.Source, role.Model, role.MaxTurns, strings.Join(role.Tools, ", "))
	}
	for _, reason := range snapshot.Rejections {
		fmt.Fprintf(&out, "\n跳过：%s", reason)
	}
	return out.String()
}

func renderAgentTask(task agent.AgentTaskSnapshot) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Agent 任务 %s · %s · %s（只读）", task.ID, task.AgentName, task.Status)
	if task.Stage != "" {
		fmt.Fprintf(&out, "\n阶段：%s", task.Stage)
	}
	if task.Summary != "" {
		fmt.Fprintf(&out, "\n摘要：%s", task.Summary)
	}
	if task.Error != "" {
		fmt.Fprintf(&out, "\n错误：%s", task.Error)
	}
	return out.String()
}

func renderAgentTaskList(tasks []agent.AgentTaskSnapshot) string {
	var out strings.Builder
	out.WriteString("本会话 Agent 任务：")
	if len(tasks) == 0 {
		out.WriteString("\n（无更多任务）")
	}
	for _, task := range tasks {
		fmt.Fprintf(&out, "\n- %s · %s · %s", task.ID, task.AgentName, task.Status)
		if task.Stage != "" {
			fmt.Fprintf(&out, " · %s", task.Stage)
		}
		if task.Summary != "" {
			fmt.Fprintf(&out, "\n  %s", task.Summary)
		}
		if task.Error != "" {
			fmt.Fprintf(&out, "\n  错误：%s", task.Error)
		}
	}
	if len(tasks) == 20 {
		out.WriteString("\n使用 /tasks next 查看下一页。")
	}
	return out.String()
}
