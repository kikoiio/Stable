package conversation

import (
	"strings"
	"testing"
)

func TestSessionProtocolRequiresProjectAndSessionIdentity(t *testing.T) {
	for _, raw := range []string{`{"op":"session_list"}`, `{"op":"session_load","project_root":"/tmp"}`, `{"op":"chat","text":"hello","unknown":true}`} {
		if _, err := decodeClient(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted invalid request %s", raw)
		}
	}
	got, err := decodeClient(strings.NewReader(`{"op":"session_load","project_root":"/tmp/project","session_id":"0123456789abcdef0123456789abcdef"}`))
	if err != nil || got.SessionID == "" {
		t.Fatalf("valid request: %+v %v", got, err)
	}
}

func TestMemoryProtocolValidation(t *testing.T) {
	valid := []string{
		`{"op":"memory_list","session_id":"0123456789abcdef0123456789abcdef"}`,
		`{"op":"memory_delete","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"project","memory_entry":"overview.md"}`,
		`{"op":"memory_clear","session_id":"0123456789abcdef0123456789abcdef"}`,
		`{"op":"memory_clear","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"user"}`,
		`{"op":"memory_clear","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"all"}`,
	}
	for _, raw := range valid {
		if _, err := decodeClient(strings.NewReader(raw)); err != nil {
			t.Errorf("rejected valid request %s: %v", raw, err)
		}
	}
	invalid := []string{
		`{"op":"memory_list"}`,
		`{"op":"memory_delete","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"all","memory_entry":"x.md"}`,
		`{"op":"memory_delete","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"project"}`,
		`{"op":"memory_clear","session_id":"0123456789abcdef0123456789abcdef","memory_scope":"project"}`,
	}
	for _, raw := range invalid {
		if _, err := decodeClient(strings.NewReader(raw)); err == nil {
			t.Errorf("accepted invalid request %s", raw)
		}
	}
}
