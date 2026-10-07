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

func TestCoordinatorModeProtocolIsSessionScopedAndOneShot(t *testing.T) {
	valid := ClientMsg{Op: "team_coordinator", SessionID: "0123456789abcdef0123456789abcdef", CoordinatorOn: true}
	if err := validateClient(valid); err != nil {
		t.Fatalf("valid coordinator mode request rejected: %v", err)
	}
	for _, invalid := range []ClientMsg{
		{Op: "team_coordinator", SessionID: "bad"},
		{Op: "team_coordinator", SessionID: valid.SessionID, RunID: "run"},
		{Op: "team_coordinator", SessionID: valid.SessionID, ProjectRoot: "/tmp"},
	} {
		if err := validateClient(invalid); err == nil {
			t.Fatalf("invalid coordinator mode request accepted: %+v", invalid)
		}
	}
}

func TestWorktreeLifecycleAndPreviewProtocolShapes(t *testing.T) {
	session := "0123456789abcdef0123456789abcdef"
	workspaceID := "1123456789abcdef0123456789abcdef"
	for _, valid := range []ClientMsg{
		{Op: "worktree_enter", SessionID: session, ID: workspaceID},
		{Op: "worktree_exit", SessionID: session},
		{Op: "worktree_export", SessionID: session, ID: workspaceID},
		{Op: "worktree_preview", SessionID: session, ID: workspaceID},
	} {
		if err := validateClient(valid); err != nil {
			t.Fatalf("valid worktree request rejected: %+v: %v", valid, err)
		}
	}
	for _, invalid := range []ClientMsg{
		{Op: "worktree_enter", SessionID: session},
		{Op: "worktree_exit", SessionID: session, ID: "workspace-1"},
		{Op: "worktree_export", SessionID: session, ID: "../candidate"},
		{Op: "worktree_preview", SessionID: session},
		{Op: "worktree_preview", SessionID: session, ID: "../candidate"},
	} {
		if err := validateClient(invalid); err == nil {
			t.Fatalf("invalid worktree request accepted: %+v", invalid)
		}
	}
}
