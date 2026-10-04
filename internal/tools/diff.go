package tools

import (
	"fmt"
	"strings"
)

const (
	diffContextLines = 3
	maxDiffLines     = 200
)

type DiffResult struct {
	Text      string
	Additions int
	Removals  int
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type diffOp struct {
	kind byte // ' ', '-', or '+'
	text string
	old  int
	new  int
}

// BuildDiff returns a compact, line-numbered diff. It handles multiple separated
// changes and caps only the rendered text; addition/removal counts remain exact.
func BuildDiff(oldContent, newContent string) DiffResult {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	ops := diffLines(oldLines, newLines)

	var additions, removals int
	changed := make([]bool, len(ops))
	for i, op := range ops {
		switch op.kind {
		case '+':
			additions++
			changed[i] = true
		case '-':
			removals++
			changed[i] = true
		}
	}

	// Keep context only around changed operations, including across an unchanged
	// run when the two changes are close enough to form one readable hunk.
	keep := make([]bool, len(ops))
	for i, isChanged := range changed {
		if !isChanged {
			continue
		}
		start, end := i-diffContextLines, i+diffContextLines
		if start < 0 {
			start = 0
		}
		if end >= len(ops) {
			end = len(ops) - 1
		}
		for j := start; j <= end; j++ {
			keep[j] = true
		}
	}

	out := make([]string, 0, min(maxDiffLines+1, len(ops)))
	truncated := false
	for i, op := range ops {
		if !keep[i] {
			continue
		}
		if len(out) >= maxDiffLines {
			truncated = true
			break
		}
		lineNo := op.old
		if op.kind == '+' {
			lineNo = op.new
		}
		out = append(out, fmt.Sprintf("%c %4d  %s", op.kind, lineNo, op.text))
	}
	if truncated {
		out = append(out, fmt.Sprintf("  ... (diff truncated at %d lines)", maxDiffLines))
	}
	return DiffResult{Text: strings.Join(out, "\n"), Additions: additions, Removals: removals}
}

func diffLines(oldLines, newLines []string) []diffOp {
	// LCS gives stable, readable edits and handles separated changes. Avoid a
	// quadratic allocation for unusually large files by using the original
	// contiguous-change strategy as a bounded fallback.
	if len(oldLines) > 2000 || len(newLines) > 2000 || len(oldLines)*len(newLines) > 4_000_000 {
		return contiguousDiff(oldLines, newLines)
	}
	dp := make([][]int, len(oldLines)+1)
	for i := range dp {
		dp[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	ops := make([]diffOp, 0)
	i, j := 0, 0
	oldNo, newNo := 1, 1
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			ops = append(ops, diffOp{' ', oldLines[i], oldNo, newNo})
			i++
			j++
			oldNo++
			newNo++
		} else if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, diffOp{'-', oldLines[i], oldNo, newNo})
			i++
			oldNo++
		} else {
			ops = append(ops, diffOp{'+', newLines[j], oldNo, newNo})
			j++
			newNo++
		}
	}
	for i < len(oldLines) {
		ops = append(ops, diffOp{'-', oldLines[i], oldNo, newNo})
		i++
		oldNo++
	}
	for j < len(newLines) {
		ops = append(ops, diffOp{'+', newLines[j], oldNo, newNo})
		j++
		newNo++
	}
	return ops
}

func contiguousDiff(oldLines, newLines []string) []diffOp {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix && oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	oldNo, newNo := 1, 1
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{' ', oldLines[i], oldNo, newNo})
		oldNo++
		newNo++
	}
	for i := prefix; i < len(oldLines)-suffix; i++ {
		ops = append(ops, diffOp{'-', oldLines[i], oldNo, newNo})
		oldNo++
	}
	for i := prefix; i < len(newLines)-suffix; i++ {
		ops = append(ops, diffOp{'+', newLines[i], oldNo, newNo})
		newNo++
	}
	for i := len(oldLines) - suffix; i < len(oldLines); i++ {
		ops = append(ops, diffOp{' ', oldLines[i], oldNo, newNo})
		oldNo++
		newNo++
	}
	return ops
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
