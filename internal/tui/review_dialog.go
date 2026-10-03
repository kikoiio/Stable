package tui

import (
	"fmt"
	"strings"

	"stable/internal/candidate"
)

func renderReview(review candidate.Review, confirmed map[string]bool, cursor int, status, errorText string, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "候选预览 %s\n正式版 %s\n候选版 %s\n预览摘要 %s\n", review.CandidateID, review.FormalDigest, review.CandidateDigest, review.Digest)
	b.WriteString("\n文件变更\n")
	for _, change := range review.Changes {
		fmt.Fprintf(&b, "%s %s\n", change.Status, change.Path)
		if change.TextDiff != "" {
			b.WriteString(sanitizeReviewText(change.TextDiff))
		} else {
			if change.BeforeDigest != "" {
				fmt.Fprintf(&b, "  before sha256 %s\n", change.BeforeDigest)
			}
			if change.AfterDigest != "" {
				fmt.Fprintf(&b, "  after  sha256 %s\n", change.AfterDigest)
			}
		}
	}
	b.WriteString("\n检查结果\n")
	for i, finding := range review.Findings {
		mark := " "
		if i == cursor {
			mark = ">"
		}
		ack := ""
		if confirmed[finding.ID] {
			ack = " [已确认]"
		}
		fmt.Fprintf(&b, "%s %s: %s (%s %s)%s\n", mark, finding.ID, finding.Result, finding.Checker, finding.Version, ack)
		if finding.Reason != "" {
			fmt.Fprintf(&b, "  原因：%s\n", sanitizeReviewText(finding.Reason))
		}
		for _, file := range finding.Files {
			fmt.Fprintf(&b, "  影响：%s\n", sanitizeReviewText(file))
		}
	}
	if status != "" {
		fmt.Fprintf(&b, "\n%s\n", sanitizeReviewText(status))
	}
	if errorText != "" {
		fmt.Fprintf(&b, "错误：%s\n", sanitizeReviewText(errorText))
	}
	fmt.Fprintf(&b, "\n←/→ 选择发现 · Space 逐项确认 · a 普通接收 · f 强制接收 · Esc 返回\n")
	return truncateReviewLines(b.String(), width)
}

func sanitizeReviewText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 {
			return r
		}
		return -1
	}, s)
}

func truncateReviewLines(s string, width int) string {
	if width < 1 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if len([]rune(line)) > width {
			lines[i] = string([]rune(line)[:max(1, width-1)]) + "…"
		}
	}
	return strings.Join(lines, "\n")
}
