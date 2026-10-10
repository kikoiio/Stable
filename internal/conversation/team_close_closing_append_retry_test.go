package conversation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCloseTeamRetriesWhenClosingFactCannotBeAppended(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "close-closing-append-retry")
	team, err := service.CreateTeam(t.Context(), request, "close-closing-append-retry")
	if err != nil {
		t.Fatal(err)
	}

	beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalMode := info.Mode().Perm()
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, originalMode); err != nil {
			t.Errorf("restore session log mode: %v", err)
		}
	})

	if _, err := service.CloseTeam(t.Context(), request, team.ID); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("close with read-only session log error=%v, want permission error", err)
	}
	afterFailureProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterFailureHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterFailureTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailureProjection.Teams[team.ID] != beforeProjection.Teams[team.ID] || len(afterFailureHistory) != len(beforeHistory) || len(afterFailureTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatalf("failed TeamClosing append changed durable state: team %+v -> %+v, history %d -> %d, events %d -> %d",
			beforeProjection.Teams[team.ID], afterFailureProjection.Teams[team.ID], len(beforeHistory), len(afterFailureHistory), len(beforeTranscript.Events), len(afterFailureTranscript.Events))
	}

	if err := os.Chmod(path, originalMode); err != nil {
		t.Fatal(err)
	}
	closed, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closed.Status != teams.TeamClosed {
		t.Fatalf("retry close = %+v, %v; want closed", closed, err)
	}
	finalProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalProjection.Teams[team.ID].Status != teams.TeamClosed {
		t.Fatalf("durable team status=%s, want closed", finalProjection.Teams[team.ID].Status)
	}
	finalHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var closingFacts, closedFacts int
	for _, event := range finalHistory {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		switch fact.Kind {
		case sessionlog.TeamClosing:
			closingFacts++
		case sessionlog.TeamClosed:
			closedFacts++
		}
	}
	if closingFacts != 1 || closedFacts != 1 {
		t.Fatalf("close facts closing/closed=%d/%d, want exactly one each", closingFacts, closedFacts)
	}
}
