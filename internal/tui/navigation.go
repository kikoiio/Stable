package tui

import (
	"fmt"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"strings"
)

type ViewMode uint8

const (
	ChatView ViewMode = iota
	SessionPickerView
	GoalPickerView
)

type NavigationState struct {
	Mode     ViewMode
	Cursor   int
	ReturnTo ViewMode
}

func renderSessions(items []sessionlog.SessionInfo, active string, cursor, width int) string {
	if len(items) == 0 {
		return "暂无会话。按 Esc 返回。"
	}
	var b strings.Builder
	for i, s := range items {
		mark := " "
		if i == cursor || (cursor < 0 && s.ID == active) {
			mark = "›"
		}
		title := s.Title
		if title == "" {
			title = s.ID
		}
		fmt.Fprintf(&b, "%s %s\n", mark, truncate(title, width-4))
	}
	return b.String()
}
func renderGoals(items []core.Goal, cursor, width int) string {
	if len(items) == 0 {
		return "暂无目标。按 Esc 返回。"
	}
	var b strings.Builder
	for i, g := range items {
		name := g.Objective
		if name == "" {
			name = g.ID
		}
		mark := " "
		if i == cursor {
			mark = "›"
		}
		fmt.Fprintf(&b, "%s %s [%s]\n", mark, truncate(name, width-8), g.Status)
		if i == cursor {
			if g.Reason != "" {
				fmt.Fprintf(&b, "  摘要：%s\n", g.Reason)
			}
			if g.EvidenceSummary != "" {
				fmt.Fprintf(&b, "  证据：%s\n", g.EvidenceSummary)
			}
		}
	}
	return b.String()
}
func truncate(s string, n int) string {
	if n < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) > n {
		if n == 1 {
			return "…"
		}
		return string(r[:n-1]) + "…"
	}
	return s
}
