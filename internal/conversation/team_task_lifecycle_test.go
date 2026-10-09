package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamTaskAssigneeMustBelongToTeam(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-owner-run")
	team, err := service.CreateTeam(context.Background(), request, "task-owners")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "invalid owner", Assignee: "member-missing"}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("create with non-member assignee = %v, want permission error", err)
	}
	tasks, err := service.ListTeamTasks(context.Background(), request, team.ID)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("rejected create persisted a task: tasks=%+v err=%v", tasks, err)
	}

	task, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	missing := "member-missing"
	if _, err := service.UpdateTeamTask(context.Background(), request, team.ID, task.ID, task.Revision, teams.TaskPatch{Assignee: &missing}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("update with non-member assignee = %v, want permission error", err)
	}
	unchanged, err := service.GetTeamTask(context.Background(), request, team.ID, task.ID)
	if err != nil || unchanged.Revision != task.Revision || unchanged.Assignee != "" {
		t.Fatalf("rejected owner update changed task: task=%+v err=%v", unchanged, err)
	}

	addTeamMessageMember(t, service, request, team.ID, "member-a", "reader")
	memberID := "member-a"
	assigned, err := service.UpdateTeamTask(context.Background(), request, team.ID, task.ID, task.Revision, teams.TaskPatch{Assignee: &memberID})
	if err != nil {
		t.Fatalf("valid team member assignment failed: %v", err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Tasks[task.ID]; got.Assignee != memberID || got.Revision != assigned.Revision {
		t.Fatalf("valid assignment was not durably replayed: %+v", got)
	}
}
