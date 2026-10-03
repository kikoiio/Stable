package tui

import (
	"fmt"
	"strings"

	"stable/internal/permission"
)

func renderApprovalDialog(requests []permission.ApprovalPrompt, selected, width int) string {
	if len(requests) == 0 {
		return "没有待授权操作。"
	}
	a := requests[clamp(selected, 0, len(requests)-1)]
	var b strings.Builder
	fmt.Fprintf(&b, "需要授权 · %d/%d\n操作：%s (%s)\n目标：%s\n项目范围：%s\n原因：%s\n请求：%s\n到期：%s\n\n", selected+1, len(requests), safeDialogText(a.Name), a.Kind, safeDialogText(a.Target), safeDialogText(a.AllowedRoot), safeDialogText(a.Reason), a.ID, a.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	b.WriteString("1 允许一次 · 2 保存精确规则 · 3 拒绝 · c 取消请求 · ↑/↓ 切换 · Esc 保持等待")
	return truncateReviewLines(b.String(), width)
}

func safeDialogText(s string) string { return sanitizeReviewText(s) }
