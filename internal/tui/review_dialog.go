package tui

import (
	"fmt"
	"strings"
	"time"

	"stable/internal/candidate"
	"stable/internal/sessionlog"
)

func renderReview(review candidate.Review, confirmed map[string]bool, cursor int, snapshots []sessionlog.SnapshotRef, rewindPick bool, rewindCursor int, rewindArmed bool, runActive bool, status, errorText string, width int) string {
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
	b.WriteString("\n快照\n")
	if len(snapshots) == 0 {
		b.WriteString("（该候选在此会话没有快照）\n")
	}
	for i, snap := range snapshots {
		mark := " "
		if rewindPick && i == rewindCursor {
			mark = ">"
		}
		label := snap.Label
		if label == "" {
			label = "checkpoint"
		}
		fmt.Fprintf(&b, "%s %s · %s · digest %s", mark, snap.CreatedAt.Local().Format(time.DateTime), label, shortDigest(snap.Digest))
		if snap.RunID != "" {
			fmt.Fprintf(&b, " · 运行 %s", snap.RunID)
		}
		b.WriteString("\n")
	}
	if rewindPick && len(snapshots) > 0 {
		target := snapshots[min(rewindCursor, len(snapshots)-1)]
		if rewindArmed {
			fmt.Fprintf(&b, "\n再次 Enter 确认回滚到快照 %s（%s），Esc 取消。\n", target.SnapshotID, shortDigest(target.Digest))
		} else {
			fmt.Fprintf(&b, "\nEnter 选择回滚目标快照，Esc 取消。\n")
		}
	} else if runActive {
		b.WriteString("\n有活动运行：回滚已禁用，请先等待运行结束或取消。\n")
	}
	if status != "" {
		fmt.Fprintf(&b, "\n%s\n", sanitizeReviewText(status))
	}
	if errorText != "" {
		fmt.Fprintf(&b, "错误：%s\n", sanitizeReviewText(errorText))
	}
	fmt.Fprintf(&b, "\n←/→ 选择发现 · Space 逐项确认 · a 普通接收 · f 强制接收 · r 回滚到快照 · Esc 返回\n")
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
