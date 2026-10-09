package tui

import (
	"strings"
	"testing"

	"stable/internal/teams"
)

func TestFormatTeamMembersDisplaysWorkspaceIDWhenPresent(t *testing.T) {
	member := teams.Member{
		ID: "member-1", TeamID: "team-1", Name: "builder", AgentName: "builder-role",
		Status: teams.MemberIdle, WorkspaceID: "workspace-opaque-123",
	}
	formatted := formatTeamMembers("team-1", []teams.Member{member})
	if !strings.Contains(formatted, "工作树：workspace-opaque-123") {
		t.Fatalf("member workspace ID missing from listing: %q", formatted)
	}

	member.WorkspaceID = ""
	formatted = formatTeamMembers("team-1", []teams.Member{member})
	if strings.Contains(formatted, "工作树：") {
		t.Fatalf("member without workspace ID got a workspace label: %q", formatted)
	}
}
