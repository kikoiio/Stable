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

func TestGoalTeamsAreIsolatedAcrossPersistedWorkItemQueries(t *testing.T) {
	ctx := context.Background()
	projectRoot := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(projectRoot, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(projectRoot, "goal team work item query isolation")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if _, err := state.CreateGoal(ctx, coreGoal("goal-query-isolation", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}

	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-query-isolation", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-query-isolation", WorkItemID: "item-b"}
	requestA := appendGoalScopeRun(t, projectRoot, "goal-query-isolation-run-a", workA)
	requestB := appendGoalScopeRun(t, projectRoot, "goal-query-isolation-run-b", workB)
	service := &Service{
		deps:       Deps{ProjectRoot: projectRoot, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
	}
	teamA, err := service.CreateTeam(ctx, requestA, "work-item-a-team")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(ctx, requestB, "work-item-b-team")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, requestA, teamA.ID, "member-a", "reader-a")
	addTeamMessageMember(t, service, requestB, teamB.ID, "member-b", "reader-b")
	for _, fixture := range []struct {
		request agent.ExecutionRequest
		team    teams.Team
		member  string
		token   string
	}{
		{requestA, teamA, "member-a", "owner-a-message"},
		{requestB, teamB, "member-b", "owner-b-message"},
	} {
		if _, err := service.SendTeamMessage(ctx, fixture.request, TeamSendRequest{
			TeamID: fixture.team.ID, Recipient: fixture.member, Body: fixture.token, Token: fixture.token,
		}); err != nil {
			t.Fatalf("authorized WorkItem %s could not send to its team: %v", fixture.request.Work.WorkItemID, err)
		}
	}

	for _, fixture := range []struct {
		name    string
		request agent.ExecutionRequest
		own     teams.Team
		foreign teams.Team
	}{
		{"item-a", requestA, teamA, teamB},
		{"item-b", requestB, teamB, teamA},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			beforeProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistoryA, err := sessionlog.TeamHistory(projectRoot, session.ID, teamA.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistoryB, err := sessionlog.TeamHistory(projectRoot, session.ID, teamB.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			beforeTranscript, err := sessionlog.Replay(projectRoot, session.ID)
			if err != nil {
				t.Fatal(err)
			}

			listed, err := service.ListTeams(ctx, fixture.request)
			if err != nil {
				t.Fatalf("list own WorkItem teams: %v", err)
			}
			if len(listed) != 1 || listed[0].ID != fixture.own.ID {
				t.Fatalf("WorkItem %s listed %+v; want only own team %s", fixture.request.Work.WorkItemID, listed, fixture.own.ID)
			}
			if _, err := service.GetTeam(ctx, fixture.request, fixture.own.ID); err != nil {
				t.Fatalf("get own team: %v", err)
			}
			if _, err := service.GetTeam(ctx, fixture.request, fixture.foreign.ID); err == nil {
				t.Fatalf("WorkItem %s read foreign team %s", fixture.request.Work.WorkItemID, fixture.foreign.ID)
			}
			if messages, err := service.ListTeamMessages(ctx, fixture.request, fixture.foreign.ID, 0, teams.MaxPageSize); err == nil || len(messages) != 0 {
				t.Fatalf("WorkItem %s read foreign team messages %+v, err=%v", fixture.request.Work.WorkItemID, messages, err)
			}
			messages, err := service.ListTeamMessages(ctx, fixture.request, fixture.own.ID, 0, teams.MaxPageSize)
			if err != nil || len(messages) != 1 || messages[0].TeamID != fixture.own.ID {
				t.Fatalf("WorkItem %s own message list = %+v, err=%v", fixture.request.Work.WorkItemID, messages, err)
			}

			afterProjection, err := sessionlog.ReplayTeams(projectRoot, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterHistoryA, err := sessionlog.TeamHistory(projectRoot, session.ID, teamA.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			afterHistoryB, err := sessionlog.TeamHistory(projectRoot, session.ID, teamB.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			afterTranscript, err := sessionlog.Replay(projectRoot, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistoryA, beforeHistoryA) || !reflect.DeepEqual(afterHistoryB, beforeHistoryB) || len(afterTranscript.Events) != len(beforeTranscript.Events) {
				t.Fatal("team query/list operations changed durable team or session facts")
			}
		})
	}
}
