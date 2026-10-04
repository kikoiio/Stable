package tui

import "fmt"

type StatusPhase uint8

const (
	StatusIdle StatusPhase = iota
	StatusLoading
	StatusError
)

type StatusState struct {
	Phase StatusPhase
	Text  string
}

func renderStatus(s StatusState, session string, mode ViewMode, width int) string {
	phase := "就绪"
	switch s.Phase {
	case StatusLoading:
		phase = "处理中…"
	case StatusError:
		phase = "错误"
	}
	view := "对话"
	if mode == SessionPickerView {
		view = "会话"
	} else if mode == GoalPickerView {
		view = "目标"
	}
	hints := "PgUp/PgDn 滚动  Enter 发送  Ctrl+J 换行  Ctrl+P/N 历史  Tab 补全  Ctrl+S 会话  Ctrl+G 目标  Ctrl+C 退出"
	if mode != ChatView {
		hints = "↑/↓ 选择  Enter 确认  Esc 返回  Ctrl+C 退出"
	}
	line := fmt.Sprintf("Stable · %s · %s · %s", truncate(session, 20), view, hints)
	if s.Text != "" {
		line += " · " + s.Text
	}
	line += " · " + phase
	return truncate(line, width)
}
