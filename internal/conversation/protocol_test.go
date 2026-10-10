package conversation

import (
	"strings"
	"testing"

	"stable/internal/agent"
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
func TestCoordinatorModeProtocolIsSessionScopedAndOneShot(t *testing.T) {
	valid := ClientMsg{Op: "team_coordinator", SessionID: "0123456789abcdef0123456789abcdef", CoordinatorOn: true, CoordinatorTeamID: "1123456789abcdef0123456789abcdef"}
	if err := validateClient(valid); err != nil {
		t.Fatalf("valid coordinator mode request rejected: %v", err)
	}
	for _, invalid := range []ClientMsg{
		{Op: "team_coordinator", SessionID: "bad"},
		{Op: "team_coordinator", SessionID: valid.SessionID, RunID: "run"},
		{Op: "team_coordinator", SessionID: valid.SessionID, ProjectRoot: "/tmp"},
		{Op: "team_coordinator", SessionID: valid.SessionID, CoordinatorOn: true},
		{Op: "team_coordinator", SessionID: valid.SessionID, CoordinatorTeamID: valid.CoordinatorTeamID},
	} {
		if err := validateClient(invalid); err == nil {
			t.Fatalf("invalid coordinator mode request accepted: %+v", invalid)
		}
	}
}

func TestRunStartCoordinatorParameterIsGoalOnly(t *testing.T) {
	validGoal := ClientMsg{
		Op: "run_start", SessionID: "0123456789abcdef0123456789abcdef",
		CoordinatorTeamID: "1123456789abcdef0123456789abcdef",
		Run: &agent.ExecutionRequest{
			RunID: "goal-run", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: "0123456789abcdef0123456789abcdef", GoalID: "goal", WorkItemID: "item"}, Intent: "coordinate",
		},
	}
	if err := validateClient(validGoal); err != nil {
		t.Fatalf("valid explicit Goal coordinator run rejected: %v", err)
	}
	invalidSession := validGoal
	invalidSession.Run = &agent.ExecutionRequest{
		RunID: "session-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: validGoal.SessionID}, Intent: "coordinate",
	}
	if err := validateClient(invalidSession); err == nil {
		t.Fatal("per-run coordinator selection was accepted for a Session run")
	}
	invalidGoalID := validGoal
	invalidGoalID.CoordinatorTeamID = "invalid"
	if err := validateClient(invalidGoalID); err == nil {
		t.Fatal("invalid Goal coordinator team ID was accepted")
	}
	invalidMode := validGoal
	invalidMode.CoordinatorOn = true
	if err := validateClient(invalidMode); err == nil {
		t.Fatal("one-shot coordinator mode flag was accepted on run_start")
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

func TestTeamResumeRoleChangeAcceptanceProtocolShape(t *testing.T) {
	session := "0123456789abcdef0123456789abcdef"
	run := "1123456789abcdef0123456789abcdef"
	team := "team-1"
	member := "member-1"
	valid := ClientMsg{Op: "team_member_resume", SessionID: session, RunID: run, TeamID: team, TeamMemberID: member, TeamAcceptRoleChange: true}
	if err := validateClient(valid); err != nil {
		t.Fatalf("valid explicit role acceptance rejected: %v", err)
	}
	invalid := valid
	invalid.Op = "team_member_spawn"
	if err := validateClient(invalid); err == nil {
		t.Fatal("role acceptance flag accepted during spawn")
	}
}
