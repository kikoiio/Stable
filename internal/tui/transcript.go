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
	for _, e := range events {
		switch e.Type {
		case sessionlog.EventMessage:
			b, _ := json.Marshal(e.Data)
			var msg sessionlog.Message
			if json.Unmarshal(b, &msg) != nil || strings.TrimSpace(msg.Text) == "" {
				continue
			}
			role := msg.Role
			if role == "assistant" {
				role = "Stable"
			}
			blocks = append(blocks, &block{role: role})
			blocks[len(blocks)-1].text.WriteString(msg.Text)
		case sessionlog.EventRunEvent:
			b, _ := json.Marshal(e.Data)
			var run sessionlog.RunEvent
			if json.Unmarshal(b, &run) != nil {
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
			} else if run.Kind == "tool_call_start" || run.Kind == "tool_call_delta" || run.Kind == "tool_call_complete" || run.Kind == "tool_exec_start" || run.Kind == "tool_exec_result" || run.Kind == "awaiting_approval" || run.Kind == "budget_exhausted" || run.Kind == "usage" || run.Kind == "retry" || run.Kind == "error" || run.Kind == "terminal" {
				label := map[string]string{"tool_call_start": "工具调用", "tool_call_delta": "工具参数", "tool_call_complete": "工具调用完成", "tool_exec_start": "工具执行", "tool_exec_result": "工具结果", "awaiting_approval": "等待授权", "budget_exhausted": "预算耗尽", "usage": "用量", "retry": "重试", "error": "模型错误", "terminal": "运行状态"}[run.Kind]
				payload, _ := json.Marshal(run.Payload)
				if run.Kind == "usage" {
					payload = []byte(formatUsage(payload))
				} else if run.Kind == "terminal" {
					var terminal struct {
						Status string `json:"status"`
					}
					_ = json.Unmarshal(payload, &terminal)
					status := map[string]string{"completed": "completed", "cancelled": "cancelled", "failed": "failed", "awaiting_tools": "awaiting tools", "budget_exhausted": "budget exhausted"}[terminal.Status]
					if status != "" {
						payload = []byte(status)
					}
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
		if part.role == "思考（仅供查看）" || strings.HasPrefix(part.role, "工具") || part.role == "等待授权" || part.role == "预算耗尽" || part.role == "用量" || part.role == "重试" || part.role == "模型错误" || part.role == "运行状态" {
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
