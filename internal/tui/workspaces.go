package tui

import (
	"fmt"
	"strings"

	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

func registerWorkspaceCommands(host *commandHost, registry *commands.Registry) {
	registry.Register(&commands.Command{Name: "worktrees", Description: "列出或管理当前会话的隔离工作树", ArgPrompt: "list | create 标签 | get ID | preview ID | keep ID | remove ID", Kind: commands.KindLocal, Local: func(args string) {
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
		case "get", "preview", "enter", "keep", "export", "remove":
			if len(fields) != 2 || !workspace.ValidID(fields[1]) {
				m.Status = workspaceUsage()
				return
			}
			request.ID = fields[1]
			switch fields[0] {
			case "get":
				request.Op = "worktree_get"
			case "preview":
				request.Op = "worktree_preview"
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
	return "用法：/worktrees list | create <标签> | get <ID> | preview <ID> | enter <ID> | exit | keep <ID> | export <ID> | remove <ID>（remove 仅在内容与基线一致时成功）"
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
				fmt.Fprintf(&b, "\n冲突 %d 项", item.ConflictCount)
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
		m.Status = "隔离工作树已创建。"
	case "worktree_remove":
		m.Status = "干净工作树已删除。"
	case "worktree_export":
		m.Status = "工作树变更已导出为候选；请通过 review_get 检查并单独接受。"
	case "worktree_preview":
		m.Status = "工作树冲突预览已更新。"
	default:
		m.Status = "工作树状态已更新。"
	}
}
