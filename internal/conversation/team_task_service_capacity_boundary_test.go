package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCreateTeamTaskRejectsBeyondServiceBoundaryWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-capacity-service-boundary")
	team, err := service.CreateTeam(context.Background(), request, "task-capacity-boundary")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < teams.MaxTeamTasks; index++ {
		if _, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: fmt.Sprintf("task %03d", index)}); err != nil {
			t.Fatalf("create task %d of %d: %v", index+1, teams.MaxTeamTasks, err)
		}
	}

	beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(beforeProjection.Tasks); got != teams.MaxTeamTasks {
		t.Fatalf("service projection has %d tasks at boundary, want %d", got, teams.MaxTeamTasks)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeReplay, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	rejected, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "task beyond limit"})
	if !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("257th service task error = %v, want ErrCapacity", err)
	}
	if rejected.ID != "" {
		t.Fatalf("rejected task returned a persisted identity: %+v", rejected)
	}

	afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) {
		t.Fatalf("rejected task changed team projection: before=%+v after=%+v", beforeProjection, afterProjection)
	}
	afterHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterHistory, beforeHistory) {
		t.Fatalf("rejected task changed team history: before=%d events after=%d", len(beforeHistory), len(afterHistory))
	}
	afterReplay, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterReplay.Events) != len(beforeReplay.Events) {
		t.Fatalf("rejected task appended session facts: before=%d after=%d", len(beforeReplay.Events), len(afterReplay.Events))
	}
}
