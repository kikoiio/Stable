package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberCannotClaimCompletedTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "task-claim-run")
	team, err := service.CreateTeam(context.Background(), leadRequest, "claim-boundary")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTeamTask(context.Background(), leadRequest, team.ID, teams.Task{Title: "finished task"})
	if err != nil {
		t.Fatal(err)
	}
	completed := teams.TaskCompleted
	task, err = service.UpdateTeamTask(context.Background(), leadRequest, team.ID, task.ID, task.Revision, teams.TaskPatch{Status: &completed})
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, leadRequest, team.ID, "member-claim", "reviewer")

	memberRequest := leadRequest
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: "member-claim", TurnID: "turn-claim"}
	assignee := "member-claim"
	if _, err := service.UpdateTeamTask(context.Background(), memberRequest, team.ID, task.ID, task.Revision, teams.TaskPatch{Assignee: &assignee}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("member claim on completed task = %v, want permission error", err)
	}

	got, err := service.GetTeamTask(context.Background(), leadRequest, team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Status != teams.TaskCompleted || got.Revision != task.Revision {
		t.Fatalf("rejected claim changed completed task: %+v", got)
	}
	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed := projection.Tasks[task.ID]; replayed.Assignee != "" || replayed.Status != teams.TaskCompleted || replayed.Revision != task.Revision {
		t.Fatalf("rejected claim changed durable task fact: %+v", replayed)
	}
}
