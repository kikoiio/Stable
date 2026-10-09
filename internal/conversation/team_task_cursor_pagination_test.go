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

func TestListTeamTasksCursorPaginationReturnsAllTasksOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-cursor-pagination")
	team, err := service.CreateTeam(context.Background(), request, "task-pages")
	if err != nil {
		t.Fatal(err)
	}

	created := make([]teams.Task, 0, teams.MaxTeamTasks)
	for i := 0; i < teams.MaxTeamTasks; i++ {
		task, createErr := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: fmt.Sprintf("task %03d", i)})
		if createErr != nil {
			t.Fatalf("create task %d: %v", i, createErr)
		}
		created = append(created, task)
	}
	expected := append([]teams.Task(nil), created...)
	sort.Slice(expected, func(i, j int) bool { return expected[i].ID < expected[j].ID })
	for i := 1; i < len(expected); i++ {
		if expected[i-1].ID == expected[i].ID {
			t.Fatalf("fixture created duplicate task ID %q", expected[i].ID)
		}
	}

	cursor := ""
	offset := 0
	for pageIndex, wantSize := range []int{teams.MaxPageSize, teams.MaxPageSize, teams.MaxTeamTasks - 2*teams.MaxPageSize} {
		page, listErr := service.ListTeamTasksPage(context.Background(), request, team.ID, cursor, teams.MaxPageSize)
		if listErr != nil {
			t.Fatalf("list page %d after %q: %v", pageIndex, cursor, listErr)
		}
		if len(page) != wantSize {
			t.Fatalf("page %d returned %d tasks, want %d", pageIndex, len(page), wantSize)
		}
		for i, task := range page {
			want := expected[offset+i]
			if task.ID != want.ID {
				t.Fatalf("page %d item %d has ID %q, want %q", pageIndex, i, task.ID, want.ID)
			}
			if i > 0 && page[i-1].ID >= task.ID {
				t.Fatalf("page %d is not strictly ID ordered at %d: %q >= %q", pageIndex, i, page[i-1].ID, task.ID)
			}
		}
		if len(page) == 0 {
			t.Fatalf("page %d unexpectedly empty before all %d tasks were read", pageIndex, teams.MaxTeamTasks)
		}
		cursor = page[len(page)-1].ID
		offset += len(page)
	}
	if offset != teams.MaxTeamTasks {
		t.Fatalf("pagination returned %d tasks, want %d", offset, teams.MaxTeamTasks)
	}
	lastPage, err := service.ListTeamTasksPage(context.Background(), request, team.ID, cursor, teams.MaxPageSize)
	if err != nil || len(lastPage) != 0 {
		t.Fatalf("page after final cursor %q = %d tasks, err=%v; want empty", cursor, len(lastPage), err)
	}
}
