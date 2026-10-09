package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCreateTeamTaskEnforcesTitleAndDescriptionByteLimitsWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-field-limits")
	team, err := service.CreateTeam(context.Background(), request, "task-field-limits")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		valid     teams.Task
		oversized teams.Task
	}{
		{
			name:      "title",
			valid:     teams.Task{Title: strings.Repeat("t", teams.MaxTaskTitleBytes)},
			oversized: teams.Task{Title: strings.Repeat("t", teams.MaxTaskTitleBytes+1)},
		},
		{
			name:      "description",
			valid:     teams.Task{Title: "description boundary", Description: strings.Repeat("d", teams.MaxTaskDescriptionBytes)},
			oversized: teams.Task{Title: "description overflow", Description: strings.Repeat("d", teams.MaxTaskDescriptionBytes+1)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := service.CreateTeamTask(context.Background(), request, team.ID, tc.valid)
			if err != nil {
				t.Fatalf("create task at exact %s limit: %v", tc.name, err)
			}
			if created.ID == "" {
				t.Fatalf("task at exact %s limit has no persisted ID: %+v", tc.name, created)
			}

			beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			beforeReplay, err := sessionlog.Replay(root, request.Work.SessionID)
			if err != nil {
				t.Fatal(err)
			}

			rejected, err := service.CreateTeamTask(context.Background(), request, team.ID, tc.oversized)
			if err == nil {
				t.Fatalf("create task over %s byte limit succeeded: %+v", tc.name, rejected)
			}
			if rejected.ID != "" {
				t.Fatalf("rejected task over %s byte limit returned an ID: %+v", tc.name, rejected)
			}

			afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterProjection, beforeProjection) {
				t.Fatalf("rejected %s overflow changed team projection", tc.name)
			}
			afterHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterHistory, beforeHistory) {
				t.Fatalf("rejected %s overflow changed team history", tc.name)
			}
			afterReplay, err := sessionlog.Replay(root, request.Work.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterReplay.Events) != len(beforeReplay.Events) {
				t.Fatalf("rejected %s overflow appended session facts: before=%d after=%d", tc.name, len(beforeReplay.Events), len(afterReplay.Events))
			}
		})
	}
}
