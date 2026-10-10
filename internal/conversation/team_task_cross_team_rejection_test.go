package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamTaskIDsCannotCrossTeamBoundaryOrBeForged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "task-cross-team-lead")
	teamA, err := service.CreateTeam(context.Background(), leadRequest, "task-scope-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(context.Background(), leadRequest, "task-scope-b")
	if err != nil {
		t.Fatal(err)
	}
	taskA, err := service.CreateTeamTask(context.Background(), leadRequest, teamA.ID, teams.Task{Title: "team A task"})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := service.CreateTeamTask(context.Background(), leadRequest, teamB.ID, teams.Task{Title: "team B task"})
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, leadRequest, teamA.ID, "member-task-scope-a", "reader")
	memberRequest := leadRequest
	memberRequest.RunID = "task-cross-team-member"
	turnID := "task-cross-team-turn"
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: teamA.ID, MemberID: "member-task-scope-a", TurnID: turnID, MemberName: "reader"}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberRequest.TeamTurn.MemberID, RunID: memberRequest.RunID, TaskID: turnID, OriginRunID: leadRequest.RunID, Status: "intent"}
	appendTeamFactForTaskScopeTest(t, service, leadRequest, teamA.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamFactForTaskScopeTest(t, service, leadRequest, teamA.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: memberRequest.RunID, WorkKind: string(leadRequest.Work.Kind), Intent: "cross-team task scope",
		TeamID: teamA.ID, TeamMemberID: memberRequest.TeamTurn.MemberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}

	beforeA, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)

	for _, actor := range []struct {
		name    string
		request agent.ExecutionRequest
	}{
		{name: "lead", request: leadRequest},
		{name: "team A member", request: memberRequest},
	} {
		t.Run(actor.name, func(t *testing.T) {
			if got, err := service.GetTeamTask(context.Background(), actor.request, teamA.ID, taskB.ID); !errors.Is(err, teams.ErrNotFound) {
				t.Fatalf("read team B task through team A = %+v, %v; want not found", got, err)
			}
			if _, err := service.UpdateTeamTask(context.Background(), actor.request, teamA.ID, taskB.ID, taskB.Revision, teams.TaskPatch{Title: stringPtr("forged cross-team update")}); !errors.Is(err, teams.ErrNotFound) {
				t.Fatalf("update team B task through team A = %v; want not found", err)
			}
			if got, err := service.GetTeamTask(context.Background(), actor.request, teamA.ID, "forged-task-id"); !errors.Is(err, teams.ErrNotFound) {
				t.Fatalf("read forged task ID = %+v, %v; want not found", got, err)
			}
			if _, err := service.UpdateTeamTask(context.Background(), actor.request, teamA.ID, "forged-task-id", 1, teams.TaskPatch{Title: stringPtr("forged task")}); !errors.Is(err, teams.ErrNotFound) {
				t.Fatalf("update forged task ID = %v; want not found", err)
			}
			if actor.name == "team A member" {
				if _, err := service.GetTeamTask(context.Background(), actor.request, teamB.ID, taskB.ID); !errors.Is(err, teams.ErrPermission) {
					t.Fatalf("member A read team B task = %v; want permission error", err)
				}
				if _, err := service.UpdateTeamTask(context.Background(), actor.request, teamB.ID, taskB.ID, taskB.Revision, teams.TaskPatch{Title: stringPtr("member cross-team update")}); !errors.Is(err, teams.ErrPermission) {
					t.Fatalf("member A updated team B task = %v; want permission error", err)
				}
			}
		})
	}

	for _, expected := range []struct {
		teamID string
		before sessionlog.TeamProjection
	}{{teamA.ID, beforeA}, {teamB.ID, beforeB}} {
		after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, expected.teamID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, expected.before) {
			t.Fatalf("rejected cross-team/forged operation changed ReplayTeams(%s): before=%+v after=%+v", expected.teamID, expected.before, after)
		}
	}
	transcript, err = sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("rejected cross-team/forged operations appended sessionlog facts: before=%d after=%d", eventCount, len(transcript.Events))
	}
	if after, err := service.GetTeamTask(context.Background(), leadRequest, teamA.ID, taskA.ID); err != nil || after.Title != "team A task" {
		t.Fatalf("team A task changed during rejected operations: %+v err=%v", after, err)
	}
	if after, err := service.GetTeamTask(context.Background(), leadRequest, teamB.ID, taskB.ID); err != nil || after.Title != "team B task" {
		t.Fatalf("team B task changed during rejected operations: %+v err=%v", after, err)
	}
}

func appendTeamFactForTaskScopeTest(t *testing.T, service *Service, request agent.ExecutionRequest, teamID, kind string, turn sessionlog.TurnFact) {
	t.Helper()
	if err := appendTeamFactLocked(service.deps.ProjectRoot, request.Work.SessionID, teamID, sessionlog.TeamEvent{Kind: kind, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
}
