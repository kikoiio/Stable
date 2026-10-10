package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTUIMemberListPaginationBoundsAndInvalidLimits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "team member list pagination")
	if err != nil {
		t.Fatal(err)
	}
	const runID = "team-member-list-parent"
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: "session", Intent: "member list fixture"}); err != nil {
		t.Fatal(err)
	}
	teamID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	teamEventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	team := teams.Team{
		ID: teamID, Name: "member-history", Scope: teams.Scope{SessionID: session.ID, WorkKind: "session", ProjectRoot: root},
		CreatorRunID: runID, Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: teamEventID, TeamID: teamID, SessionID: session.ID, Kind: sessionlog.TeamCreated,
		Revision: 1, ActorID: teams.Lead, ActorRunID: runID, Team: &team,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range teams.MaxPageSize + 1 {
		appendTUIHistoricalTeamMember(t, root, session.ID, runID, &team, i)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	model := New("", root)
	model.ActiveSession = session.ID
	model.Events = transcript.Events

	model = runTUITeamMembersCommand(t, model, teamID, "")
	if len(model.TeamMembers) != teams.DefaultPageSize {
		t.Fatalf("default TUI page size=%d, want %d", len(model.TeamMembers), teams.DefaultPageSize)
	}
	assertTeamMemberPageOrderedUnique(t, model.TeamMembers)

	model = runTUITeamMembersCommand(t, model, teamID, "100")
	if len(model.TeamMembers) != teams.MaxPageSize {
		t.Fatalf("explicit TUI page size=%d, want %d", len(model.TeamMembers), teams.MaxPageSize)
	}
	assertTeamMemberPageOrderedUnique(t, model.TeamMembers)

	for _, invalid := range []string{"0", "101"} {
		before := append([]teams.Member(nil), model.TeamMembers...)
		model = runTUITeamMembersCommand(t, model, teamID, invalid)
		if len(model.TeamMembers) != len(before) {
			t.Fatalf("invalid limit %s changed the current member page", invalid)
		}
		for i := range before {
			if model.TeamMembers[i].ID != before[i].ID {
				t.Fatalf("invalid limit %s changed member ordering at %d", invalid, i)
			}
		}
		if model.Status == "" {
			t.Fatalf("invalid limit %s did not show usage feedback", invalid)
		}
	}
}

func appendTUIHistoricalTeamMember(t *testing.T, root, sessionID, runID string, team *teams.Team, index int) {
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
	append := func(kind, actor string, revision uint64, current *teams.Member) {
		t.Helper()
		eventID, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		if _, appendErr := sessionlog.Append(root, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
			ID: eventID, TeamID: team.ID, SessionID: sessionID, Kind: kind, Revision: revision,
			ActorID: actor, ActorRunID: runID, Member: current,
		}); appendErr != nil {
			t.Fatalf("append %s for member %s: %v", kind, id, appendErr)
		}
	}
	append(sessionlog.TeamMemberAdded, teams.Lead, team.Revision+1, &member)
	team.Revision++
	member.Status = teams.MemberStopped
	member.Revision++
	append(sessionlog.TeamMemberState, "service", team.Revision+1, &member)
	team.Revision++
}

func runTUITeamMembersCommand(t *testing.T, model Model, teamID, limit string) Model {
	t.Helper()
	commandText := "/team " + teamID + " members"
	if limit != "" {
		commandText += " " + limit
	}
	model.Composer.SetValue(commandText)
	updated, command := model.submitComposer()
	if command != nil {
		t.Fatal("team members listing unexpectedly became asynchronous")
	}
	result, ok := updated.(Model)
	if !ok {
		t.Fatalf("team members command returned %T, want Model", updated)
	}
	return result
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
