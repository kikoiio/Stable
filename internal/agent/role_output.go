package agent

import (
	"strings"
	"unicode/utf8"
)

const roleOutputOmitted = "[agent role instructions omitted]"

// SanitizeRoleOutput removes verbatim role instructions from public child
// output. It recognizes complete/trimmed bodies, substantive individual lines,
// and line prefixes clipped by the summary budget. It preserves unrelated
// findings. It cannot identify paraphrases or arbitrary short fragments.
// Definitions and inspected output are bounded by the 64 KiB task input limit.
func SanitizeRoleOutput(text, body string) string {
	if text == "" || body == "" {
		return text
	}
	text = truncateBytesUTF8(text, 64<<10)
	body = truncateBytesUTF8(body, 64<<10)
	text = strings.ReplaceAll(text, body, roleOutputOmitted)
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return text
	}
	text = strings.ReplaceAll(text, trimmed, roleOutputOmitted)
	for _, rawLine := range strings.Split(trimmed, "\n") {
		line := strings.TrimSpace(rawLine)
		if len(line) < 16 {
			continue // Avoid suppressing unrelated short/common findings.
		}
		text = strings.ReplaceAll(text, line, roleOutputOmitted)
		text = omitClippedRoleLine(text, line)
	}
	return text
}

func omitClippedRoleLine(text, line string) string {
	// Only a substantial prefix can establish a verbatim clipped line. The
	// comparison ends at the bounded output limit, not at the full role size.
	prefix := line[:16]
	for offset := 0; offset < len(text); {
		found := strings.Index(text[offset:], prefix)
		if found < 0 {
			break
		}
		start := offset + found
		matched := 16
		for matched < len(line) && start+matched < len(text) && line[matched] == text[start+matched] {
			matched++
		}
		for matched > 0 && !utf8.ValidString(text[start:start+matched]) {
			matched--
		}
		end := start + matched
		clipped := end == len(text)
		if strings.HasPrefix(text[end:], "…") {
			end += len("…")
			clipped = true
		} else if strings.HasPrefix(text[end:], "...") {
			end += 3
			clipped = true
		}
		if matched >= 16 && clipped {
			text = text[:start] + roleOutputOmitted + text[end:]
			offset = start + len(roleOutputOmitted)
		} else {
			offset = start + 16
		}
	}
	return text
}
