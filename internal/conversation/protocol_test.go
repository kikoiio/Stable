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
