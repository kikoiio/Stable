package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamQueriesBindRunToPersistedWorkRef(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "team query scope fixture")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "query-goal-a", WorkItemID: "query-item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "query-goal-b", WorkItemID: "query-item-b"}
	for _, fixture := range []struct {
		id   string
		work agent.WorkRef
	}{
		{id: "query-goal-a", work: workA},
		{id: "query-goal-b", work: workB},
	} {
		goalRoot := filepath.Join(root, fixture.id+"-root")
		if err := os.MkdirAll(goalRoot, 0700); err != nil {
			t.Fatal(err)
		}
		goal := coreGoal(fixture.id, goalRoot, session.ID)
		goal.Objective = "team query scope fixture"
		if _, err := state.CreateGoal(context.Background(), goal); err != nil {
			t.Fatal(err)
		}
	}

	requestA := agent.ExecutionRequest{RunID: "query-run-a", Work: workA}
	requestB := agent.ExecutionRequest{RunID: "query-run-b", Work: workB}
	for _, request := range []agent.ExecutionRequest{requestA, requestB} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: request.RunID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID,
			WorkItemID: request.Work.WorkItemID, Intent: "team query scope fixture",
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
	}
	teamA, err := service.CreateTeam(context.Background(), requestA, "query-team-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(context.Background(), requestB, "query-team-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		request agent.ExecutionRequest
		team    teams.Team
		member  string
	}{
		{request: requestA, team: teamA, member: "query-member-a"},
		{request: requestB, team: teamB, member: "query-member-b"},
	} {
		addTeamMessageMember(t, service, fixture.request, fixture.team.ID, fixture.member, fixture.member)
		if _, err := service.CreateTeamTask(context.Background(), fixture.request, fixture.team.ID, teams.Task{Title: "scoped task"}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.SendTeamMessage(context.Background(), fixture.request, TeamSendRequest{
			TeamID: fixture.team.ID, Recipient: fixture.member, Body: "scoped message", Token: "message-" + fixture.member,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, fixture := range []struct {
		request agent.ExecutionRequest
		team    teams.Team
		wantID  string
	}{
		{request: requestA, team: teamA, wantID: "query-item-a"},
		{request: requestB, team: teamB, wantID: "query-item-b"},
	} {
		tasks, err := service.ListTeamTasks(context.Background(), fixture.request, fixture.team.ID)
		if err != nil || len(tasks) != 1 || tasks[0].Title != "scoped task" {
			t.Fatalf("valid run %q could not list its scoped tasks: tasks=%+v err=%v", fixture.request.RunID, tasks, err)
		}
		got, err := service.GetTeamTask(context.Background(), fixture.request, fixture.team.ID, tasks[0].ID)
		if err != nil || got.ID != tasks[0].ID {
			t.Fatalf("valid run %q could not get its scoped task: task=%+v err=%v", fixture.request.RunID, got, err)
		}
		messages, err := service.ListTeamMessages(context.Background(), fixture.request, fixture.team.ID, 0, teams.MaxPageSize)
		if err != nil || len(messages) != 1 || messages[0].Body != "scoped message" {
			t.Fatalf("valid run %q could not list its scoped messages: messages=%+v err=%v", fixture.request.RunID, messages, err)
		}
		requests, err := service.ListTeamRequestsPage(context.Background(), fixture.request, fixture.team.ID, "", teams.MaxPageSize)
		if err != nil || len(requests) != 0 {
			t.Fatalf("valid run %q could not list its scoped requests: requests=%+v err=%v", fixture.request.RunID, requests, err)
		}
		if fixture.request.Work.WorkItemID != fixture.wantID {
			t.Fatalf("fixture work item = %q, want %q", fixture.request.Work.WorkItemID, fixture.wantID)
		}
	}

	before, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	forged := requestA
	forged.Work = workB
	teamBTasks, err := service.ListTeamTasks(context.Background(), requestB, teamB.ID)
	if err != nil || len(teamBTasks) != 1 {
		t.Fatalf("valid B fixture task query failed: tasks=%+v err=%v", teamBTasks, err)
	}
	for _, query := range []struct {
		name string
		call func() error
	}{
		{name: "task get", call: func() error {
			_, err := service.GetTeamTask(context.Background(), forged, teamB.ID, teamBTasks[0].ID)
			return err
		}},
		{name: "task list", call: func() error {
			_, err := service.ListTeamTasks(context.Background(), forged, teamB.ID)
			return err
		}},
		{name: "messages", call: func() error {
			_, err := service.ListTeamMessages(context.Background(), forged, teamB.ID, 0, teams.MaxPageSize)
			return err
		}},
		{name: "requests", call: func() error {
			_, err := service.ListTeamRequestsPage(context.Background(), forged, teamB.ID, "", teams.MaxPageSize)
			return err
		}},
	} {
		if err := query.call(); err == nil {
			t.Errorf("Run A with forged WorkRef B was accepted by %s query", query.name)
		}
	}
	after, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Events, after.Events) {
		t.Fatal("rejected forged queries changed persisted session facts")
	}
}
