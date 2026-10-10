package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"stable/internal/sessionlog"
)

type Transcript struct {
	Viewport      viewport.Model
	Events        []sessionlog.Event
	Width, Height int
}

func projectTranscript(events []sessionlog.Event, width int, color bool) string {
	type block struct {
		role string
		text strings.Builder
	}
	var blocks []*block
	runs := map[string]*block{}
	delegations := map[string]*block{}
	backgroundRuns := map[string]bool{}
	for _, event := range events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if decodeEventData(event.Data, &started) == nil && (started.AgentTaskID != "" || started.TeamID != "") {
				backgroundRuns[started.RunID] = true
			}
		}
	}
	// First pass: pair tool calls with their results and questions with
	// their replies, so recovery display can flag interrupted calls and
	// already-answered questions instead of leaving both ambiguous.
	answered := map[string]bool{}
	toolResults := map[string]sessionlog.ToolResult{}
	for _, e := range events {
		switch e.Type {
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeEventData(e.Data, &result) == nil && result.CallID != "" {
				toolResults[result.CallID] = result
			}
		case sessionlog.EventReply:
			var reply sessionlog.QuestionReply
			if decodeEventData(e.Data, &reply) == nil {
				answered[reply.QuestionID] = true
			}
		}
	}
	for _, e := range events {
		switch e.Type {
		case sessionlog.EventMessage:
			var msg sessionlog.Message
			if decodeEventData(e.Data, &msg) != nil || strings.TrimSpace(msg.Text) == "" {
				continue
			}
			role := msg.Role
			if role == "assistant" {
				role = "Stable"
			}
			blocks = append(blocks, &block{role: role})
			blocks[len(blocks)-1].text.WriteString(msg.Text)
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			if decodeEventData(e.Data, &call) != nil {
				continue
			}
			role := "工具调用"
			if _, ok := toolResults[call.CallID]; !ok {
				role = "工具调用（未配对，可能已中断）"
			}
			part := &block{role: role}
			part.text.WriteString(call.Name)
			if call.Input != nil {
				raw, _ := json.Marshal(call.Input)
				part.text.WriteString(" " + truncate(string(raw), 400))
			}
			blocks = append(blocks, part)
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeEventData(e.Data, &result) != nil {
				continue
			}
			part := &block{role: "工具结果"}
			if result.Error != "" {
				part.text.WriteString("失败：" + truncate(result.Error, 400))
			} else {
				raw, _ := json.Marshal(result.Result)
				part.text.WriteString(truncate(string(raw), 400))
			}
			blocks = append(blocks, part)
		case sessionlog.EventBoundary:
			var boundary sessionlog.Boundary
			if decodeEventData(e.Data, &boundary) != nil {
				continue
			}
			part := &block{role: "上下文压缩"}
			scope := boundary.EffectiveScope()
			fmt.Fprintf(&part.text, "摘要：%s\n（覆盖 %s 序号 %d–%d", truncate(boundary.Summary, 600), scope, boundary.FromSeq, boundary.ToSeq)
			if boundary.RunID != "" {
				fmt.Fprintf(&part.text, "，运行 %s", boundary.RunID)
			}
			part.text.WriteString("）")
			blocks = append(blocks, part)
		case sessionlog.EventSnapshot:
			var snap sessionlog.SnapshotRef
			if decodeEventData(e.Data, &snap) != nil {
				continue
			}
			part := &block{role: "快照"}
			label := snap.Label
			if label == "" {
				label = "checkpoint"
			}
			fmt.Fprintf(&part.text, "%s · 候选 %s · digest %s", label, snap.CandidateID, shortDigest(snap.Digest))
			if snap.RunID != "" {
				fmt.Fprintf(&part.text, " · 运行 %s", snap.RunID)
			}
			blocks = append(blocks, part)
		case sessionlog.EventRewind:
			var rewind sessionlog.RewindRecord
			if decodeEventData(e.Data, &rewind) != nil {
				continue
			}
			part := &block{role: "回滚"}
			switch rewind.Status {
			case sessionlog.RewindCompleted:
				fmt.Fprintf(&part.text, "成功：候选 %s 已回滚到快照 %s", rewind.CandidateID, rewind.SnapshotID)
			case sessionlog.RewindFailed:
				fmt.Fprintf(&part.text, "失败：候选 %s 回滚到快照 %s 未完成", rewind.CandidateID, rewind.SnapshotID)
				if rewind.Error != "" {
					fmt.Fprintf(&part.text, "：%s", truncate(rewind.Error, 300))
				}
			default:
				fmt.Fprintf(&part.text, "待处理：候选 %s 回滚到快照 %s", rewind.CandidateID, rewind.SnapshotID)
			}
			blocks = append(blocks, part)
		case sessionlog.EventQuestion:
			var question sessionlog.PendingQuestion
			if decodeEventData(e.Data, &question) != nil {
				continue
			}
			part := &block{role: "待答问题"}
			part.text.WriteString(truncate(question.Prompt, 600))
			if answered[question.QuestionID] {
				part.text.WriteString("\n（已回答）")
			} else {
				part.text.WriteString("\n（待回答 · /reply 答复）")
			}
			blocks = append(blocks, part)
		case sessionlog.EventReply:
			var reply sessionlog.QuestionReply
			if decodeEventData(e.Data, &reply) != nil {
				continue
			}
			part := &block{role: "问题回复"}
			part.text.WriteString(truncate(reply.ReplyText, 600))
			blocks = append(blocks, part)
		case sessionlog.EventPlanMode:
			var mode sessionlog.PlanMode
			if decodeEventData(e.Data, &mode) != nil {
				continue
			}
			part := &block{role: "计划模式"}
			if mode.Mode == sessionlog.PlanModePlan {
				part.text.WriteString("进入计划模式")
			} else {
				part.text.WriteString("退出计划模式")
			}
			switch mode.Reason {
			case sessionlog.PlanModeReasonUserToggle:
				part.text.WriteString("（用户切换）")
			case sessionlog.PlanModeReasonPlanApproved:
				part.text.WriteString("（计划已批准）")
			case sessionlog.PlanModeReasonPlanCancelled:
				part.text.WriteString("（计划审批取消）")
			}
			blocks = append(blocks, part)
		case sessionlog.EventPlanApproval:
			var approval sessionlog.PlanApprovalRecord
			if decodeEventData(e.Data, &approval) != nil {
				continue
			}
			part := &block{role: "计划审批"}
			switch approval.Status {
			case sessionlog.PlanApprovalSubmitted:
				fmt.Fprintf(&part.text, "待审批：%s", approval.PlanPath)
			case sessionlog.PlanApprovalApprovedAuto:
				fmt.Fprintf(&part.text, "已批准（自动接受）：%s", approval.PlanPath)
			case sessionlog.PlanApprovalApprovedManual:
				fmt.Fprintf(&part.text, "已批准（逐次确认）：%s", approval.PlanPath)
			case sessionlog.PlanApprovalFeedback:
				fmt.Fprintf(&part.text, "反馈继续改：%s", approval.PlanPath)
				if approval.Feedback != "" {
					fmt.Fprintf(&part.text, "\n反馈：%s", truncate(approval.Feedback, 300))
				}
			case sessionlog.PlanApprovalCancelled:
				fmt.Fprintf(&part.text, "已取消：%s", approval.PlanPath)
			}
			blocks = append(blocks, part)
		case sessionlog.EventTodo:
			var update sessionlog.TodoUpdate
			if decodeEventData(e.Data, &update) != nil {
				continue
			}
			part := &block{role: "任务清单"}
			if len(update.Tasks) == 0 {
				part.text.WriteString("（空）")
			}
			for _, task := range update.Tasks {
				fmt.Fprintf(&part.text, "[%s] %s\n", task.Status, task.Subject)
			}
			blocks = append(blocks, part)
		case sessionlog.EventSkillInventory:
			var inv sessionlog.SkillInventory
			if decodeEventData(e.Data, &inv) != nil {
				continue
			}
			part := &block{role: "技能清单"}
			if len(inv.Skills) == 0 {
				part.text.WriteString("（无技能）")
			}
			for _, info := range inv.Skills {
				fmt.Fprintf(&part.text, "- %s（%s）\n", info.Name, info.Source)
			}
			blocks = append(blocks, part)
		case sessionlog.EventSkillDelta:
			var delta sessionlog.SkillDelta
			if decodeEventData(e.Data, &delta) != nil {
				continue
			}
			part := &block{role: "技能新增"}
			for _, info := range delta.Added {
				fmt.Fprintf(&part.text, "%s ", info.Name)
			}
			blocks = append(blocks, part)
		case sessionlog.EventSkillInvoked:
			var invoked sessionlog.SkillInvoked
			if decodeEventData(e.Data, &invoked) != nil {
				continue
			}
			part := &block{role: "技能激活"}
			fmt.Fprintf(&part.text, "%s（%s·%s）", invoked.Name, invoked.Source, invoked.Entry)
			blocks = append(blocks, part)
		case sessionlog.EventHookFired:
			var fired sessionlog.HookFired
			if decodeEventData(e.Data, &fired) != nil {
				continue
			}
			part := &block{role: "Hook"}
			mark := "✓"
			if !fired.Success {
				mark = "✗"
			}
			reject := ""
			if fired.Rejected {
				reject = "[已拒绝]"
			}
			fmt.Fprintf(&part.text, "`%s`(%s) %s%s: %s", fired.HookID, fired.Event, mark, reject, fired.Output)
			blocks = append(blocks, part)
		case sessionlog.EventHookReload:
			var reload sessionlog.HookReload
			if decodeEventData(e.Data, &reload) != nil {
				continue
			}
			part := &block{role: "Hooks 重载"}
			fmt.Fprintf(&part.text, "%d → %d", reload.Before, reload.After)
			blocks = append(blocks, part)
		case sessionlog.EventMCPReload:
			var reload sessionlog.MCPReload
			if decodeEventData(e.Data, &reload) != nil {
				continue
			}
			part := &block{role: "MCP 重载"}
			fmt.Fprintf(&part.text, "%d → %d（%s）", reload.Before, reload.After, reload.Trigger)
			if len(reload.Rejections) > 0 {
				fmt.Fprintf(&part.text, "；跳过 %d 项", len(reload.Rejections))
			}
			blocks = append(blocks, part)
		case sessionlog.EventMCPServer:
			var server sessionlog.MCPServer
			if decodeEventData(e.Data, &server) != nil {
				continue
			}
			part := &block{role: "MCP 服务器"}
			fmt.Fprintf(&part.text, "%s（%s·%s，工具:%d）", server.Name, server.Source, server.State, server.ToolCount)
			if server.Error != "" {
				part.text.WriteString("：" + server.Error)
			}
			blocks = append(blocks, part)
		case sessionlog.EventRunEvent:
			var run sessionlog.RunEvent
			if decodeEventData(e.Data, &run) != nil {
				continue
			}
			if backgroundRuns[run.RunID] && run.Kind != "delegation_event" {
				continue
			}
			if run.Kind == "text_delta" || run.Kind == "thinking_delta" {
				key := run.RunID + ":" + run.Kind
				part := runs[key]
				if part == nil {
					role := "Stable"
					if run.Kind == "thinking_delta" {
						role = "思考（仅供查看）"
					}
					part = &block{role: role}
					runs[key] = part
					blocks = append(blocks, part)
				}
				var payload struct {
					Text string `json:"text"`
				}
				pb, _ := json.Marshal(run.Payload)
				if json.Unmarshal(pb, &payload) == nil {
					part.text.WriteString(payload.Text)
				}
			} else if run.Kind == "delegation_event" {
				var delegation struct {
					BatchID  string `json:"batch_id"`
					TaskID   string `json:"task_id"`
					TaskName string `json:"task_name"`
					Status   string `json:"status"`
					Stage    string `json:"stage"`
					Summary  string `json:"summary"`
					Error    string `json:"error"`
				}
				payload, _ := json.Marshal(run.Payload)
				if json.Unmarshal(payload, &delegation) != nil {
					continue
				}
				key := delegation.BatchID + ":" + delegation.TaskID
				part := delegations[key]
				if part == nil {
					part = &block{role: "协作任务"}
					delegations[key] = part
					blocks = append(blocks, part)
				}
				part.text = strings.Builder{}
				fmt.Fprintf(&part.text, "%s · %s", delegation.TaskName, delegation.Status)
				if delegation.Stage != "" {
					fmt.Fprintf(&part.text, " · %s", delegation.Stage)
				}
				if delegation.Summary != "" {
					fmt.Fprintf(&part.text, " · %s", delegation.Summary)
				}
				if delegation.Error != "" {
					fmt.Fprintf(&part.text, " · %s", delegation.Error)
				}
			} else if run.Kind == "tool_call_start" || run.Kind == "tool_call_delta" || run.Kind == "tool_call_complete" || run.Kind == "tool_exec_start" || run.Kind == "tool_exec_result" || run.Kind == "awaiting_approval" || run.Kind == "budget_exhausted" || run.Kind == "usage" || run.Kind == "retry" || run.Kind == "error" || run.Kind == "terminal" {
				label := map[string]string{"tool_call_start": "工具调用", "tool_call_delta": "工具参数", "tool_call_complete": "工具调用完成", "tool_exec_start": "工具执行", "tool_exec_result": "工具结果", "awaiting_approval": "等待授权", "budget_exhausted": "预算耗尽", "usage": "用量", "retry": "重试", "error": "模型错误", "terminal": "运行状态"}[run.Kind]
				payload, _ := json.Marshal(run.Payload)
				if run.Kind == "usage" {
					payload = []byte(formatUsage(payload))
				} else if run.Kind == "terminal" {
					var terminal struct {
						Status  string `json:"status"`
						Summary string `json:"summary"`
						Reason  string `json:"reason"`
					}
					_ = json.Unmarshal(payload, &terminal)
					status := map[string]string{"completed": "completed", "cancelled": "cancelled", "failed": "failed", "awaiting_tools": "awaiting tools", "budget_exhausted": "budget exhausted", "interrupted": "interrupted"}[terminal.Status]
					var text strings.Builder
					if status != "" {
						text.WriteString(status)
					}
					if terminal.Summary != "" {
						if text.Len() > 0 {
							text.WriteString("\n")
						}
						text.WriteString(terminal.Summary)
					}
					if terminal.Reason != "" {
						if text.Len() > 0 {
							text.WriteString("\n")
						}
						text.WriteString(terminal.Reason)
					}
					payload = []byte(text.String())
				}
				part := &block{role: label}
				part.text.Write(payload)
				blocks = append(blocks, part)
			}
		}
	}
	var out []string
	for _, part := range blocks {
		text := strings.TrimSpace(part.text.String())
		if text == "" {
			continue
		}
		if part.role == "思考（仅供查看）" || strings.HasPrefix(part.role, "工具") || part.role == "等待授权" || part.role == "预算耗尽" || part.role == "用量" || part.role == "重试" || part.role == "模型错误" || part.role == "运行状态" || part.role == "上下文压缩" || part.role == "快照" || part.role == "回滚" || part.role == "待答问题" || part.role == "问题回复" {
			out = append(out, part.role+"\n"+text)
		} else {
			out = append(out, part.role+"\n"+renderMarkdown(text, max(1, width-10), color))
		}
	}
	if len(out) == 0 {
		return "空会话。输入消息后回车开始聊天。"
	}
	return strings.Join(out, "\n\n")
}

// decodeEventData re-decodes a sessionlog event payload, which arrives as a
// generic map after JSON round-trips.
func decodeEventData(data any, out any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func formatUsage(payload []byte) string {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(payload, &envelope) == nil && len(envelope.Usage) > 0 {
		payload = envelope.Usage
	}
	var usage struct {
		Input       *int `json:"input_tokens"`
		Output      *int `json:"output_tokens"`
		CacheRead   *int `json:"cache_read_tokens"`
		CacheCreate *int `json:"cache_creation_tokens"`
	}
	if json.Unmarshal(payload, &usage) != nil {
		return "用量字段不可用"
	}
	value := func(n *int) string {
		if n == nil {
			return "unavailable"
		}
		return fmt.Sprint(*n)
	}
	return fmt.Sprintf("input=%s, output=%s, cache_read=%s, cache_creation=%s", value(usage.Input), value(usage.Output), value(usage.CacheRead), value(usage.CacheCreate))
}

func (t *Transcript) SetSize(width, height int) {
	wasAtBottom := t.Viewport.AtBottom()
	oldScrollPercent := t.Viewport.ScrollPercent()
	t.Width, t.Height = max(1, width), max(1, height)
	if t.Viewport.Width == 0 {
		t.Viewport = viewport.New(t.Width, t.Height)
	} else {
		t.Viewport.Width, t.Viewport.Height = t.Width, t.Height
	}
	t.refresh(false)
	if wasAtBottom {
		t.Viewport.GotoBottom()
	} else {
		content, _ := (markdownTranscriptRenderer{}).Render(t.Events, t.Width)
		maxOffset := max(0, strings.Count(content, "\n")+1-t.Viewport.Height+t.Viewport.Style.GetVerticalFrameSize())
		offset := int(oldScrollPercent * float64(maxOffset))
		if maxOffset > 0 && offset >= maxOffset {
			offset = maxOffset - 1
		}
		t.Viewport.SetYOffset(offset)
	}
}
func (t *Transcript) SetEvents(events []sessionlog.Event) {
	t.Events = append([]sessionlog.Event(nil), events...)
	t.refresh(true)
}
func (t *Transcript) refresh(toEnd bool) {
	content, _ := (markdownTranscriptRenderer{}).Render(t.Events, t.Width)
	t.Viewport.SetContent(content)
	if toEnd {
		t.Viewport.GotoBottom()
	}
}
func (t Transcript) View() string   { return t.Viewport.View() }
func (t Transcript) String() string { return fmt.Sprint(t.View()) }
