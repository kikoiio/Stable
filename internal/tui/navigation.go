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
	SearchResultsView
)

type NavigationState struct {
	Mode     ViewMode
	Cursor   int
	ReturnTo ViewMode
	// Filter narrows the session picker by title or ID as the user types.
	Filter string
}

// filterSessions returns the sessions whose title or ID contains the filter,
// case-insensitively. An empty filter keeps the recency order untouched.
func filterSessions(items []sessionlog.SessionInfo, filter string) []sessionlog.SessionInfo {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return items
	}
	out := []sessionlog.SessionInfo{}
	for _, s := range items {
		if strings.Contains(strings.ToLower(s.Title), filter) || strings.Contains(strings.ToLower(s.ID), filter) {
			out = append(out, s)
		}
	}
	return out
}

func renderSessions(items []sessionlog.SessionInfo, active string, cursor, width int, filter string) string {
	filtered := filterSessions(items, filter)
	if filter != "" {
		if len(filtered) == 0 {
			return "过滤：" + filter + "\n没有匹配的会话。按 Esc 返回。"
		}
	}
	if len(filtered) == 0 {
		return "暂无会话。按 Esc 返回。"
	}
	var b strings.Builder
	if filter != "" {
		fmt.Fprintf(&b, "过滤：%s\n", filter)
	}
	for i, s := range filtered {
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

// renderSearchResults lists cross-session content hits with their snippets
// and reports sessions whose logs could not be scanned.
func renderSearchResults(hits []sessionlog.SearchHit, corrupt []sessionlog.SearchError, cursor, width int) string {
	var b strings.Builder
	if len(hits) == 0 {
		b.WriteString("没有匹配的会话。")
	} else {
		for i, hit := range hits {
			mark := " "
			if i == cursor {
				mark = "›"
			}
			title := hit.Session.Title
			if title == "" {
				title = hit.Session.ID
			}
			field := "标题"
			if hit.Field != "title" {
				field = "内容"
			}
			fmt.Fprintf(&b, "%s %s · %s\n", mark, truncate(title, max(1, width-12)), field)
			if hit.Snippet != "" {
				fmt.Fprintf(&b, "  %s\n", truncate(hit.Snippet, width-4))
			}
		}
	}
	for _, c := range corrupt {
		fmt.Fprintf(&b, "会话 %s 日志损坏，无法搜索：%s\n", c.SessionID, truncate(c.Err, max(1, width-24)))
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
