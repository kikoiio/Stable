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

func TestTeamTaskDependenciesRejectSelfAndForeignTeamIDsWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-dependency-scope-lead")
	teamA, err := service.CreateTeam(context.Background(), request, "dependency-scope-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(context.Background(), request, "dependency-scope-b")
	if err != nil {
		t.Fatal(err)
	}
	taskA, err := service.CreateTeamTask(context.Background(), request, teamA.ID, teams.Task{Title: "team A task"})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := service.CreateTeamTask(context.Background(), request, teamB.ID, teams.Task{Title: "team B task"})
	if err != nil {
		t.Fatal(err)
	}

	beforeA, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	initialEventCount := len(transcript.Events)

	selfDependency := []string{taskA.ID}
	foreignDependency := []string{taskB.ID}
	if _, err := service.UpdateTeamTask(context.Background(), request, teamA.ID, taskA.ID, taskA.Revision, teams.TaskPatch{BlockedBy: &selfDependency}); !errors.Is(err, teams.ErrDependency) {
		t.Fatalf("self-dependency patch error = %v, want dependency error", err)
	}
	if _, err := service.CreateTeamTask(context.Background(), request, teamA.ID, teams.Task{Title: "foreign dependency create", BlockedBy: foreignDependency}); !errors.Is(err, teams.ErrDependency) {
		t.Fatalf("foreign-team dependency create error = %v, want dependency error", err)
	}
	if _, err := service.UpdateTeamTask(context.Background(), request, teamA.ID, taskA.ID, taskA.Revision, teams.TaskPatch{BlockedBy: &foreignDependency}); !errors.Is(err, teams.ErrDependency) {
		t.Fatalf("foreign-team dependency patch error = %v, want dependency error", err)
	}

	afterA, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterB, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterA, beforeA) || !reflect.DeepEqual(afterB, beforeB) {
		t.Fatalf("rejected dependency operations changed replay projections: team A before/after=%+v/%+v; team B before/after=%+v/%+v", beforeA, afterA, beforeB, afterB)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != initialEventCount {
		t.Fatalf("rejected dependency operations appended facts: before=%d after=%d", initialEventCount, len(transcript.Events))
	}
	currentA, err := service.GetTeamTask(context.Background(), request, teamA.ID, taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentB, err := service.GetTeamTask(context.Background(), request, teamB.ID, taskB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentA.Revision != taskA.Revision || len(currentA.BlockedBy) != 0 || currentB.Revision != taskB.Revision || len(currentB.BlockedBy) != 0 {
		t.Fatalf("rejected dependency operations changed task state: A=%+v B=%+v", currentA, currentB)
	}
}
