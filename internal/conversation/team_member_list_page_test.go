package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCoordinatorTeamMemberListBoundsAndOrdersHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-member-list-page")
	team, err := service.CreateTeam(context.Background(), request, "member-history")
	if err != nil {
		t.Fatal(err)
	}

	for i := range teams.MaxPageSize + 1 {
		appendHistoricalTeamMember(t, root, request.Work.SessionID, request.RunID, &team, i)
	}

	defaultPage := executeTeamMemberList(t, service, request, team.ID, 0, false)
	if len(defaultPage) != teams.DefaultPageSize {
		t.Fatalf("default member page size=%d, want %d", len(defaultPage), teams.DefaultPageSize)
	}
	assertTeamMemberPageOrderedUnique(t, defaultPage)

	maximumPage := executeTeamMemberList(t, service, request, team.ID, teams.MaxPageSize+1, true)
	if len(maximumPage) != teams.MaxPageSize {
		t.Fatalf("oversized member page size=%d, want capped at %d", len(maximumPage), teams.MaxPageSize)
	}
	assertTeamMemberPageOrderedUnique(t, maximumPage)

	repeatedPage := executeTeamMemberList(t, service, request, team.ID, teams.MaxPageSize+1, true)
	for i := range maximumPage {
		if repeatedPage[i].ID != maximumPage[i].ID {
			t.Fatalf("member ordering changed at %d: first=%q repeated=%q", i, maximumPage[i].ID, repeatedPage[i].ID)
		}
	}
}

func executeTeamMemberList(t *testing.T, service *Service, request agent.ExecutionRequest, teamID string, limit int, includeLimit bool) []teams.Member {
	t.Helper()
	args := map[string]any{"team_id": teamID}
	if includeLimit {
		args["limit"] = limit
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.ExecuteTeamTool(context.Background(), request, llm.ToolUse{ID: "list-members", Name: "team_member_list", Arguments: encoded})
	if err != nil || outcome.IsError || outcome.Status != agent.ToolSucceeded {
		t.Fatalf("team_member_list outcome=%+v, err=%v", outcome, err)
	}
	var members []teams.Member
	if err := json.Unmarshal([]byte(outcome.Content), &members); err != nil {
		t.Fatalf("decode member list %q: %v", outcome.Content, err)
	}
	return members
}

func appendHistoricalTeamMember(t *testing.T, root, sessionID, runID string, team *teams.Team, index int) teams.Member {
	t.Helper()
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "reader"
	if index%2 == 1 {
		name = "analyst"
	}
	member := teams.Member{ID: id, TeamID: team.ID, Name: name, AgentName: "explore", RoleHash: "fixture-role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	appendEvent := func(kind, actor string, revision uint64, state *teams.Member) {
		t.Helper()
		eventID, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		_, appendErr := sessionlog.Append(root, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
			ID: eventID, TeamID: team.ID, SessionID: sessionID, Kind: kind, Revision: revision,
			ActorID: actor, ActorRunID: runID, Member: state,
		})
		if appendErr != nil {
			t.Fatalf("append %s for member %s: %v", kind, id, appendErr)
		}
	}
	appendEvent(sessionlog.TeamMemberAdded, teams.Lead, team.Revision+1, &member)
	team.Revision++
	member.Status = teams.MemberStopped
	member.Revision++
	appendEvent(sessionlog.TeamMemberState, "service", team.Revision+1, &member)
	team.Revision++
	return member
}

func assertTeamMemberPageOrderedUnique(t *testing.T, members []teams.Member) {
	t.Helper()
	seen := make(map[string]struct{}, len(members))
	for i, member := range members {
		if member.ID == "" {
			t.Fatalf("member %d has an empty ID", i)
		}
		if _, duplicate := seen[member.ID]; duplicate {
			t.Fatalf("member page contains duplicate ID %q", member.ID)
		}
		seen[member.ID] = struct{}{}
		if i > 0 {
			previous := members[i-1]
			if previous.Name > member.Name || previous.Name == member.Name && previous.ID >= member.ID {
				t.Fatalf("member page is not ordered by name then ID at %d: %q/%q before %q/%q", i, previous.Name, previous.ID, member.Name, member.ID)
			}
		}
	}
}
