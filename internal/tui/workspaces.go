package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

func registerWorkspaceCommands(host *commandHost, registry *commands.Registry) {
	registry.Register(&commands.Command{Name: "worktrees", Description: "列出或管理当前会话的隔离工作树", ArgPrompt: "list | create 标签 | get ID | preview ID | resolve ID | keep ID | remove ID | discard ID", Kind: commands.KindLocal, Local: func(args string) {
		m := host.model
		if sessionlog.ValidateID(m.ActiveSession) != nil {
			m.Status = "先选择一个会话。"
			return
		}
		fields := strings.Fields(args)
		if len(fields) == 0 {
			fields = []string{"list"}
		}
		request := conversation.ClientMsg{SessionID: m.ActiveSession, WorkKind: "session"}
		switch fields[0] {
		case "list":
			if len(fields) != 1 {
				m.Status = workspaceUsage()
				return
			}
			request.Op = "worktree_list"
		case "create":
			label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "create"))
			if label == "" || m.ActiveRunID == "" {
				m.Status = "创建工作树需要标签和活动 lead run；在运行期间执行 /worktrees create <标签>。"
				return
			}
			request.Op, request.RunID, request.Text = "worktree_create", m.ActiveRunID, label
		case "preview":
			if (len(fields) != 2 && (len(fields) != 4 || fields[2] != "after")) || !workspace.ValidID(fields[1]) {
				m.Status = workspaceUsage()
				return
			}
			request.Op, request.ID = "worktree_preview", fields[1]
			if len(fields) == 4 {
				request.ConflictAfter = fields[3]
			}
		case "get", "resolve", "discard", "enter", "keep", "export", "remove":
			if len(fields) != 2 || !workspace.ValidID(fields[1]) {
				m.Status = workspaceUsage()
				return
			}
			request.ID = fields[1]
			switch fields[0] {
			case "get":
				request.Op = "worktree_get"
			case "resolve":
				request.Op = "worktree_preview"
			case "discard":
				request.Op = "worktree_discard_preview"
			case "enter":
				request.Op = "worktree_enter"
			case "keep":
				request.Op = "worktree_keep"
			case "export":
				request.Op = "worktree_export"
			case "remove":
				request.Op = "worktree_remove"
			}
		case "exit":
			if len(fields) != 1 {
				m.Status = workspaceUsage()
				return
			}
			request.Op = "worktree_exit"
		default:
			m.Status = workspaceUsage()
			return
		}
		m.Err = nil
		m.Status = "正在处理工作树请求…"
		m.Composer.SetValue("")
		m.recordHistory(host.raw)
		host.send(requestCmd(m.Socket, request))
	}})
}

func workspaceUsage() string {
	return "用法：/worktrees list | create <标签> | get <ID> | preview <ID> [after <路径>] | resolve <ID> | enter <ID> | exit | keep <ID> | export <ID> | remove <ID> | discard <ID>（discard 会先显示保留、导出与丢弃确认）"
}

func formatWorktreeMessages(messages []conversation.ServerMsg) string {
	var b strings.Builder
	for _, msg := range messages {
		if msg.Type == "worktree_list" {
			fmt.Fprintf(&b, "工作树（%d）\n", len(msg.Worktrees))
			for _, item := range msg.Worktrees {
				fmt.Fprintf(&b, "- %s · %s · %s · generation %d · %d 个变更\n", item.Label, item.ID, item.State, item.Generation, item.ChangedFiles)
			}
		}
		if msg.Type == "worktree" && msg.Worktree != nil {
			item := msg.Worktree
			fmt.Fprintf(&b, "工作树 %s · %s · %s · generation %d · %d 个变更", item.Label, item.ID, item.State, item.Generation, item.ChangedFiles)
			if item.ConflictCount > 0 {
				fmt.Fprintf(&b, "\n冲突 %d 项 · 已选择 %d 项", item.ConflictCount, item.ResolvedCount)
				for _, path := range item.Conflicts {
					fmt.Fprintf(&b, "\n- %s", path)
				}
				if len(item.Conflicts) < item.ConflictCount {
					fmt.Fprintf(&b, "\n另有 %d 项未显示", item.ConflictCount-len(item.Conflicts))
				}
			}
			if item.BaselineDigest != "" || item.FormalDigest != "" || item.WorkspaceDigest != "" {
				fmt.Fprintf(&b, "\nB %s · F %s · W %s", item.BaselineDigest, item.FormalDigest, item.WorkspaceDigest)
			}
			if item.CandidateID != "" {
				fmt.Fprintf(&b, " · candidate %s", item.CandidateID)
			}
			if item.DiscardID != "" {
				fmt.Fprintf(&b, "\n删除预览：将丢弃 %d 个变更；可保留或导出后再决定。", item.ChangedFiles)
			}
			if item.Error != "" {
				fmt.Fprintf(&b, "\n状态说明：%s", item.Error)
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

func applyWorktreeMessages(m *Model, op string, messages []conversation.ServerMsg) {
	for _, msg := range messages {
		if msg.Type == "worktree_list" {
			m.Worktrees = append([]workspace.Snapshot(nil), msg.Worktrees...)
		}
		if msg.Type == "worktree" && msg.Worktree != nil {
			if op == "worktree_preview" && msg.Worktree.ConflictCount > 0 && len(msg.Worktree.Conflicts) > 0 {
				m.WorktreeDialog = &worktreeDecisionDialog{Mode: "resolve", Snapshot: *msg.Worktree, Choices: map[string]string{}}
			}
			if op == "worktree_discard_preview" {
				m.WorktreeDialog = &worktreeDecisionDialog{Mode: "discard", Snapshot: *msg.Worktree}
			}
			found := false
			for i := range m.Worktrees {
				if m.Worktrees[i].ID == msg.Worktree.ID {
					m.Worktrees[i], found = *msg.Worktree, true
					break
				}
			}
			if !found {
				m.Worktrees = append(m.Worktrees, *msg.Worktree)
			}
		}
	}
	text := formatWorktreeMessages(messages)
	if op == "worktree_create" && text != "" {
		text += "\n源项目的本地 settings、hooks 未复制或执行，.worktreeinclude 规则未应用；如需，请先检查，再在隔离工作树内手动配置。"
	}
	if text == "" {
		m.Status = "工作树请求已完成。"
		return
	}
	m.Events = append(m.Events, sessionlog.Event{Type: sessionlog.EventMessage, Data: sessionlog.Message{Role: "系统", Text: text, Kind: "text"}})
	m.Transcript.SetEvents(m.Events)
	switch op {
	case "worktree_list":
		m.Status = fmt.Sprintf("工作树列表已更新（%d）。", len(m.Worktrees))
	case "worktree_create":
		m.Status = "工作树已创建；源 settings/hooks 未复制或执行，.worktreeinclude 未应用。需要时请先检查，再在隔离工作树内手动配置。"
	case "worktree_remove":
		m.Status = "干净工作树已删除。"
	case "worktree_discard":
		m.Status = "已按用户确认丢弃并删除工作树。"
	case "worktree_resolve":
		m.Status = "逐路径选择已记录；全部冲突处理后可导出候选并单独审阅接受。"
	case "worktree_export":
		m.Status = "工作树变更已导出为候选；请通过 review_get 检查并单独接受。"
	case "worktree_preview":
		m.Status = "工作树冲突预览已更新。"
	default:
		m.Status = "工作树状态已更新。"
	}
}

type worktreeDecisionDialog struct {
	Mode     string
	Snapshot workspace.Snapshot
	Cursor   int
	Choices  map[string]string
	Armed    bool
}

func (m Model) handleWorktreeDecisionKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	dialog := m.WorktreeDialog
	if dialog == nil {
		return m, nil
	}
	if dialog.Snapshot.SessionID != m.ActiveSession {
		m.WorktreeDialog = nil
		m.Status = "工作树确认所属会话已变化，请重新预览。"
		return m, nil
	}
	request := conversation.ClientMsg{SessionID: m.ActiveSession, ID: dialog.Snapshot.ID, WorkKind: "session"}
	if key.String() == "esc" {
		m.WorktreeDialog = nil
		m.Status = "已取消工作树决策。"
		return m, nil
	}
	if dialog.Mode == "discard" {
		switch key.String() {
		case "k":
			request.Op = "worktree_keep"
		case "e":
			request.Op = "worktree_export"
		case "d":
			dialog.Armed = true
			m.Status = "按 Enter 确认丢弃预览中的变更，Esc 取消。"
			return m, nil
		case "enter":
			if !dialog.Armed {
				return m, nil
			}
			request.Op, request.DecisionID, request.PreviewDigest, request.WorktreeGeneration = "worktree_discard", dialog.Snapshot.DiscardID, dialog.Snapshot.DiscardDigest, dialog.Snapshot.Generation
		default:
			return m, nil
		}
		m.WorktreeDialog = nil
		return m, requestCmd(m.Socket, request)
	}
	paths := dialog.Snapshot.Conflicts
	if len(paths) == 0 {
		m.WorktreeDialog = nil
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		dialog.Cursor = max(0, dialog.Cursor-1)
	case "down", "j":
		dialog.Cursor = min(len(paths)-1, dialog.Cursor+1)
	case "w":
		dialog.Choices[paths[dialog.Cursor]] = workspace.UseWorkspace
	case "f":
		dialog.Choices[paths[dialog.Cursor]] = workspace.UseFormal
	case "enter":
		if len(dialog.Choices) != len(paths) {
			m.Status = "请逐路径明确选择工作树版本（w）或正式版本（f）。"
			return m, nil
		}
		request.Op, request.WorktreePreviewID, request.WorktreeGeneration, request.ConflictChoices = "worktree_resolve", dialog.Snapshot.PreviewID, dialog.Snapshot.Generation, dialog.Choices
		m.worktreeNextPage = dialog.Snapshot.ConflictNext
		m.WorktreeDialog = nil
		return m, requestCmd(m.Socket, request)
	}
	return m, nil
}

func (m Model) renderWorktreeDecision() string {
	d := m.WorktreeDialog
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "工作树 %s · %s · generation %d\n", d.Snapshot.Label, d.Snapshot.ID, d.Snapshot.Generation)
	if d.Mode == "discard" {
		fmt.Fprintf(&b, "将丢弃 %d 个变更并删除这项工作树。\n", d.Snapshot.ChangedFiles)
		for _, path := range d.Snapshot.DiscardPaths {
			fmt.Fprintf(&b, "- %s\n", path)
		}
		if d.Snapshot.ChangedFiles > len(d.Snapshot.DiscardPaths) {
			fmt.Fprintf(&b, "另有 %d 个变更；确认绑定完整内容 digest。\n", d.Snapshot.ChangedFiles-len(d.Snapshot.DiscardPaths))
		}
		b.WriteString("k 保留 · e 导出候选 · d 准备丢弃 · Esc 取消\n")
		if d.Armed {
			b.WriteString("Enter 确认丢弃并删除；内容变化会拒绝旧确认。\n")
		}
	} else {
		fmt.Fprintf(&b, "逐路径选择（共 %d 项，已记录 %d 项）\nB %s\nF %s\nW %s\n", d.Snapshot.ConflictCount, d.Snapshot.ResolvedCount, d.Snapshot.BaselineDigest, d.Snapshot.FormalDigest, d.Snapshot.WorkspaceDigest)
		start := max(0, d.Cursor-4)
		for i := start; i < min(len(d.Snapshot.Conflicts), start+10); i++ {
			marker := " "
			if i == d.Cursor {
				marker = ">"
			}
			path := d.Snapshot.Conflicts[i]
			fmt.Fprintf(&b, "%s %s · %s\n", marker, path, d.Choices[path])
		}
		b.WriteString("↑/↓ 选择路径 · w 当前工作树版本 · f 当前正式版本 · Enter 记录本页 · Esc 取消\n导出后仍需单独审阅和接受候选。\n")
	}
	b.WriteString(m.Status)
	if m.Err != nil {
		fmt.Fprintf(&b, "\n错误：%v", m.Err)
	}
	return b.String()
}
