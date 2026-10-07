package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeRoleOutputKeepsFindingsAndRemovesVerbatimEchoes(t *testing.T) {
	body := "Keep this private role instruction out of public output.\nSecret project investigation direction stays internal.\n"
	cases := []struct {
		name, text string
		secret     []string
	}{
		{"complete", "Finding: config.yaml.\n" + body, []string{"private role", "Secret project"}},
		{"trimmed", strings.TrimSpace(body) + "\nFinding: config.yaml.", []string{"private role", "Secret project"}},
		{"line", "Finding: config.yaml.\nSecret project investigation direction stays internal.", []string{"Secret project"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeRoleOutput(tc.text, body)
			for _, secret := range tc.secret {
				if strings.Contains(got, secret) {
					t.Fatalf("role echo survived: %q", got)
				}
			}
			if !strings.Contains(got, "config.yaml") || !strings.Contains(got, roleOutputOmitted) {
				t.Fatalf("useful finding lost: %q", got)
			}
		})
	}
	plain := "config.yaml contains three sections and uses valid syntax."
	if got := SanitizeRoleOutput(plain, body); got != plain {
		t.Fatalf("unrelated output changed: %q", got)
	}
}

func TestSanitizeRoleOutputRemovesClippedLongRolePrefixes(t *testing.T) {
	for _, body := range []string{strings.Repeat("PRIVATE_LONG_ROLE_MARKER ", 1000), strings.Repeat("私密角色指令必须留在内部。", 1000)} {
		for _, suffix := range []string{"", "…", "..."} {
			prefix := truncateBytesUTF8(body, 8192-len(suffix))
			got := SanitizeRoleOutput("Useful finding: board.kicad_sch.\n"+prefix+suffix, body)
			if !strings.Contains(got, "board.kicad_sch") || !strings.Contains(got, roleOutputOmitted) || strings.Contains(got, body[:16]) || !utf8.ValidString(got) {
				t.Fatalf("clipped echo survived: bytes=%d value=%q", len(got), got)
			}
		}
	}
}

func TestSanitizeRoleOutputPreservesDistinctSharedPrefix(t *testing.T) {
	body := "Inspect the project architecture and keep role instructions private."
	text := "Inspect the project README and return relevant file paths."
	if got := SanitizeRoleOutput(text, body); got != text {
		t.Fatalf("distinct finding suppressed: %q", got)
	}
}
