package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/teams"
)

func TestListTeamTasksClampsPageAtMaxPageSize(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-query-bound")
	team, err := service.CreateTeam(context.Background(), request, "task-query")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i <= teams.MaxPageSize; i++ {
		if _, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "query task"}); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}

	defaultPage, err := service.ListTeamTasks(context.Background(), request, team.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage) != teams.DefaultPageSize {
		t.Fatalf("default task query returned %d tasks, want %d", len(defaultPage), teams.DefaultPageSize)
	}
	assertTeamTaskQueryIDsSortedUnique(t, defaultPage)

	maximumPage, err := service.ListTeamTasks(context.Background(), request, team.ID, teams.MaxPageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(maximumPage) != teams.MaxPageSize {
		t.Fatalf("maximum task query returned %d tasks, want exactly %d", len(maximumPage), teams.MaxPageSize)
	}
	assertTeamTaskQueryIDsSortedUnique(t, maximumPage)
}

func assertTeamTaskQueryIDsSortedUnique(t *testing.T, tasks []teams.Task) {
	t.Helper()
	seen := make(map[string]struct{}, len(tasks))
	for i, task := range tasks {
		if task.ID == "" {
			t.Fatalf("task %d has an empty ID", i)
		}
		if _, duplicate := seen[task.ID]; duplicate {
			t.Fatalf("task query returned duplicate ID %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		if i > 0 && tasks[i-1].ID >= task.ID {
			t.Fatalf("task IDs are not strictly increasing at %d: %q >= %q", i, tasks[i-1].ID, task.ID)
		}
	}
}
