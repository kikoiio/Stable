package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTUIMemberListCursorPaginationReturnsAllMembersOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "team member cursor pagination")
	if err != nil {
		t.Fatal(err)
	}
	const runID = "team-member-cursor-parent"
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: "session", Intent: "member cursor fixture"}); err != nil {
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
	team := teams.Team{ID: teamID, Name: "member-history", Scope: teams.Scope{SessionID: session.ID, WorkKind: "session", ProjectRoot: root}, CreatorRunID: runID, Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC()}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventTeam, sessionlog.TeamEvent{ID: teamEventID, TeamID: teamID, SessionID: session.ID, Kind: sessionlog.TeamCreated, Revision: 1, ActorID: teams.Lead, ActorRunID: runID, Team: &team}); err != nil {
		t.Fatal(err)
	}
	for i := range teams.MaxPageSize + 1 {
		appendTUIHistoricalTeamMember(t, root, session.ID, runID, &team, i)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ProjectTeams(transcript)
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]teams.Member, 0, teams.MaxPageSize+1)
	for _, member := range projection.Members {
		if member.TeamID == teamID {
			expected = append(expected, member)
		}
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Name == expected[j].Name {
			return expected[i].ID < expected[j].ID
		}
		return expected[i].Name < expected[j].Name
	})
	if len(expected) != teams.MaxPageSize+1 {
		t.Fatalf("fixture has %d members, want %d", len(expected), teams.MaxPageSize+1)
	}

	model := New("", root)
	model.ActiveSession = session.ID
	model.Events = transcript.Events
	model = runTUITeamMembersCommand(t, model, teamID, "100")
	assertTUIMemberCursorPage(t, model.TeamMembers, expected[:teams.MaxPageSize])
	model = runTUITeamMembersCommand(t, model, teamID, "100 "+expected[99].ID)
	assertTUIMemberCursorPage(t, model.TeamMembers, expected[100:])
	model = runTUITeamMembersCommand(t, model, teamID, "100 "+expected[100].ID)
	if len(model.TeamMembers) != 0 {
		t.Fatalf("page after final member cursor returned %d members, want empty", len(model.TeamMembers))
	}

	before := append([]teams.Member(nil), model.TeamMembers...)
	model = runTUITeamMembersCommand(t, model, teamID, "100 unknown-member-cursor")
	if !strings.Contains(model.Status, "cursor") {
		t.Fatalf("unknown member cursor was not rejected with cursor feedback: %q", model.Status)
	}
	if len(model.TeamMembers) != len(before) {
		t.Fatalf("invalid cursor changed the current member page: got %d, want %d", len(model.TeamMembers), len(before))
	}
}

func assertTUIMemberCursorPage(t *testing.T, got, want []teams.Member) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("member page size=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("member page item %d ID=%q, want %q", i, got[i].ID, want[i].ID)
		}
	}
}
