package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/teams"
)

func TestListTeamsClampsPageAndOrdersClosedHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-list-query-bounds")
	for i := range 101 {
		name := fmt.Sprintf("history-%03d", i)
		team, err := service.CreateTeam(context.Background(), request, name)
		if err != nil {
			t.Fatalf("create team %d: %v", i, err)
		}
		closed, err := service.CloseTeam(context.Background(), request, team.ID)
		if err != nil {
			t.Fatalf("close team %d: %v", i, err)
		}
		if closed.Status != teams.TeamClosed {
			t.Fatalf("team %d status=%q, want closed", i, closed.Status)
		}
	}

	defaultPage, err := service.ListTeams(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage) != teams.DefaultPageSize {
		t.Fatalf("default team page size=%d, want %d", len(defaultPage), teams.DefaultPageSize)
	}
	maxPage, err := service.ListTeams(context.Background(), request, 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(maxPage) != teams.MaxPageSize {
		t.Fatalf("oversized team page size=%d, want cap %d", len(maxPage), teams.MaxPageSize)
	}
	if !reflect.DeepEqual(defaultPage, maxPage[:teams.DefaultPageSize]) {
		t.Fatal("default page does not match the first page of the capped result")
	}
	secondRead, err := service.ListTeams(context.Background(), request, 101)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(maxPage, secondRead) {
		t.Fatal("repeated team list query changed order or contents")
	}
	seen := make(map[string]bool, len(maxPage))
	for i, team := range maxPage {
		if team.Status != teams.TeamClosed {
			t.Fatalf("listed team %s status=%q, want closed history", team.ID, team.Status)
		}
		if team.ID == "" || seen[team.ID] {
			t.Fatalf("team page contains empty or duplicate ID at index %d: %q", i, team.ID)
		}
		seen[team.ID] = true
		if i == 0 {
			continue
		}
		previous := maxPage[i-1]
		if team.CreatedAt.Before(previous.CreatedAt) || team.CreatedAt.Equal(previous.CreatedAt) && team.ID <= previous.ID {
			t.Fatalf("team page is not strictly ordered by created time then ID: previous=%+v current=%+v", previous, team)
		}
	}
}
