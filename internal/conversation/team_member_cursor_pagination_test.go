package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/teams"
)

func TestCoordinatorTeamMemberCursorPaginationReturnsAllMembersOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-member-cursor-pagination")
	team, err := service.CreateTeam(context.Background(), request, "member-pages")
	if err != nil {
		t.Fatal(err)
	}

	const total = teams.MaxPageSize + 1
	expected := make([]teams.Member, 0, total)
	for i := range total {
		expected = append(expected, appendHistoricalTeamMember(t, root, request.Work.SessionID, request.RunID, &team, i))
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Name == expected[j].Name {
			return expected[i].ID < expected[j].ID
		}
		return expected[i].Name < expected[j].Name
	})

	seen := make(map[string]bool, total)
	cursor := ""
	offset := 0
	for pageIndex, wantSize := range []int{teams.MaxPageSize, total - teams.MaxPageSize} {
		page := executeTeamMemberListAfter(t, service, request, team.ID, cursor, teams.MaxPageSize)
		if len(page) != wantSize {
			t.Fatalf("page %d size=%d, want %d", pageIndex, len(page), wantSize)
		}
		for i, member := range page {
			want := expected[offset+i]
			if member.ID != want.ID || member.Name != want.Name {
				t.Fatalf("page %d item %d = %q/%q, want %q/%q", pageIndex, i, member.Name, member.ID, want.Name, want.ID)
			}
			if seen[member.ID] {
				t.Fatalf("pagination returned duplicate member ID %q", member.ID)
			}
			seen[member.ID] = true
		}
		offset += len(page)
		cursor = page[len(page)-1].ID
	}
	if offset != total || len(seen) != total {
		t.Fatalf("pagination returned %d members and %d unique IDs, want %d", offset, len(seen), total)
	}
	if page := executeTeamMemberListAfter(t, service, request, team.ID, cursor, teams.MaxPageSize); len(page) != 0 {
		t.Fatalf("page after final cursor %q returned %d members, want empty", cursor, len(page))
	}

	outcome, err := executeTeamMemberListTool(context.Background(), service, request, team.ID, "unknown-member-cursor", teams.MaxPageSize)
	if err != nil || !outcome.IsError || outcome.Status != agent.ToolFailed {
		t.Fatalf("unknown member cursor outcome=%+v err=%v, want rejected tool call", outcome, err)
	}
}

func executeTeamMemberListAfter(t *testing.T, service *Service, request agent.ExecutionRequest, teamID, afterMemberID string, limit int) []teams.Member {
	t.Helper()
	outcome, err := executeTeamMemberListTool(context.Background(), service, request, teamID, afterMemberID, limit)
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
		t.Fatalf("team_member_list outcome=%+v, err=%v", outcome, err)
	}
	var members []teams.Member
	if err := json.Unmarshal([]byte(outcome.Content), &members); err != nil {
		t.Fatalf("decode member list %q: %v", outcome.Content, err)
	}
	return members
}

func executeTeamMemberListTool(ctx context.Context, service *Service, request agent.ExecutionRequest, teamID, afterMemberID string, limit int) (agent.ToolOutcome, error) {
	args := map[string]any{"team_id": teamID, "after_member_id": afterMemberID, "limit": limit}
	encoded, err := json.Marshal(args)
	if err != nil {
		return agent.ToolOutcome{}, err
	}
	return service.ExecuteTeamTool(ctx, request, llm.ToolUse{ID: "list-members-after", Name: "team_member_list", Arguments: encoded})
}
