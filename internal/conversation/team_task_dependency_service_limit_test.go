package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamTaskServiceRejectsDependencyCountOverLimitWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-dependency-count-limit")
	team, err := service.CreateTeam(context.Background(), request, "dependency-count-limit")
	if err != nil {
		t.Fatal(err)
	}

	prerequisites := make([]string, teams.MaxTaskDependencies+1)
	for index := range prerequisites {
		task, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "prerequisite"})
		if err != nil {
			t.Fatalf("create prerequisite %d: %v", index+1, err)
		}
		prerequisites[index] = task.ID
	}
	accepted, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{
		Title: "exact dependency boundary", BlockedBy: append([]string(nil), prerequisites[:teams.MaxTaskDependencies]...),
	})
	if err != nil {
		t.Fatalf("create task with exactly %d dependencies: %v", teams.MaxTaskDependencies, err)
	}
	if len(accepted.BlockedBy) != teams.MaxTaskDependencies {
		t.Fatalf("accepted dependency count=%d, want %d", len(accepted.BlockedBy), teams.MaxTaskDependencies)
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
	tooMany := append(append([]string(nil), accepted.BlockedBy...), prerequisites[teams.MaxTaskDependencies])
	if _, err := service.UpdateTeamTask(context.Background(), request, team.ID, accepted.ID, accepted.Revision, teams.TaskPatch{BlockedBy: &tooMany}); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("update task with %d dependencies = %v, want ErrCapacity", len(tooMany), err)
	}

	afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) {
		t.Fatal("rejected dependency overflow changed team projection")
	}
	afterHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterHistory, beforeHistory) {
		t.Fatal("rejected dependency overflow changed team history")
	}
	afterTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatalf("rejected dependency overflow appended session facts: %d -> %d", len(beforeTranscript.Events), len(afterTranscript.Events))
	}
	current, err := service.GetTeamTask(context.Background(), request, team.ID, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != accepted.Revision || !reflect.DeepEqual(current.BlockedBy, accepted.BlockedBy) {
		t.Fatalf("rejected dependency overflow changed task: before=%+v after=%+v", accepted, current)
	}
}
