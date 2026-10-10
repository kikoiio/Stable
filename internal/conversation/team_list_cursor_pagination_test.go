package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"stable/internal/teams"
)

func TestListTeamsCursorPaginationReturnsAllTeamsOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-cursor-pagination")

	const total = 101
	expected := make([]teams.Team, 0, total)
	for i := range total {
		created, err := service.CreateTeam(context.Background(), request, fmt.Sprintf("history-%03d", i))
		if err != nil {
			t.Fatalf("create team %d: %v", i, err)
		}
		closed, err := service.CloseTeam(context.Background(), request, created.ID)
		if err != nil {
			t.Fatalf("close team %d: %v", i, err)
		}
		if closed.Status != teams.TeamClosed {
			t.Fatalf("team %d status=%q, want closed", i, closed.Status)
		}
		expected = append(expected, closed)
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].CreatedAt.Equal(expected[j].CreatedAt) {
			return expected[i].ID < expected[j].ID
		}
		return expected[i].CreatedAt.Before(expected[j].CreatedAt)
	})

	seen := make(map[string]bool, total)
	cursor := ""
	offset := 0
	for pageIndex, wantSize := range []int{teams.MaxPageSize, total - teams.MaxPageSize} {
		page, err := service.ListTeamsPage(context.Background(), request, cursor, teams.MaxPageSize)
		if err != nil {
			t.Fatalf("list page %d after %q: %v", pageIndex, cursor, err)
		}
		if len(page) != wantSize {
			t.Fatalf("page %d size=%d, want %d", pageIndex, len(page), wantSize)
		}
		for i, team := range page {
			want := expected[offset+i]
			if team.ID != want.ID || !team.CreatedAt.Equal(want.CreatedAt) || team.Status != teams.TeamClosed {
				t.Fatalf("page %d item %d = %+v, want ID=%s created=%s closed", pageIndex, i, team, want.ID, want.CreatedAt)
			}
			if team.ID == "" || seen[team.ID] {
				t.Fatalf("pagination returned empty or duplicate team ID at index %d: %q", offset+i, team.ID)
			}
			seen[team.ID] = true
			if i > 0 {
				previous := page[i-1]
				if team.CreatedAt.Before(previous.CreatedAt) || team.CreatedAt.Equal(previous.CreatedAt) && team.ID <= previous.ID {
					t.Fatalf("page %d is not strictly ordered by created time then ID: previous=%+v current=%+v", pageIndex, previous, team)
				}
			}
		}
		offset += len(page)
		cursor = page[len(page)-1].ID
	}
	if offset != total || len(seen) != total {
		t.Fatalf("pagination returned %d teams and %d unique IDs, want %d", offset, len(seen), total)
	}
	lastPage, err := service.ListTeamsPage(context.Background(), request, cursor, teams.MaxPageSize)
	if err != nil || len(lastPage) != 0 {
		t.Fatalf("page after final cursor %q = %d teams, err=%v; want empty", cursor, len(lastPage), err)
	}
}
