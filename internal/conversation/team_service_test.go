package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func teamServiceFixture(t *testing.T, root, runID string) (*Service, agent.ExecutionRequest) {
	t.Helper()
	session, err := sessionlog.Create(root, "team fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: string(agent.WorkSession), Intent: "team service fixture"}); err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{ProjectRoot: root}, activeRuns: map[string]string{runID: session.ID}}
	request := agent.ExecutionRequest{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}}
	return service, request
}

func TestTeamLifecycleIsBoundToTrustedWorkScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-a")
	created, err := service.CreateTeam(context.Background(), request, "Research")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "research" || created.Scope.SessionID != request.Work.SessionID || created.Scope.ProjectRoot != root {
		t.Fatalf("created team has unexpected scope: %+v", created)
	}
	if _, err = service.CreateTeam(context.Background(), request, "research"); err == nil {
		t.Fatal("duplicate active team name was accepted")
	}
	if _, err = service.GetTeam(context.Background(), request, created.ID); err != nil {
		t.Fatal(err)
	}
	forged := request
	forged.Work.SessionID = "session-other"
	if _, err = service.GetTeam(context.Background(), forged, created.ID); err == nil {
		t.Fatal("team query accepted a forged session scope")
	}
	closed, err := service.CloseTeam(context.Background(), request, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != teams.TeamClosed || closed.Revision != 3 {
		t.Fatalf("close transition = %s revision %d, want closed revision 3", closed.Status, closed.Revision)
	}
	listed, err := service.ListTeams(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Status != teams.TeamClosed {
		t.Fatalf("closed history was not retained: %+v", listed)
	}
}

func TestSameTeamNameIsIsolatedAcrossValidSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	firstService, firstRequest := teamServiceFixture(t, root, "run-first")
	secondService, secondRequest := teamServiceFixture(t, root, "run-second")
	firstTeam, err := firstService.CreateTeam(context.Background(), firstRequest, "Research")
	if err != nil {
		t.Fatal(err)
	}
	secondTeam, err := secondService.CreateTeam(context.Background(), secondRequest, "research")
	if err != nil {
		t.Fatalf("same team name in another valid session was rejected: %v", err)
	}
	if firstTeam.ID == secondTeam.ID || firstTeam.Scope.SessionID == secondTeam.Scope.SessionID {
		t.Fatalf("teams did not receive independent identities and scopes: first=%+v second=%+v", firstTeam, secondTeam)
	}
	for _, fixture := range []struct {
		service *Service
		request agent.ExecutionRequest
		own     teams.Team
		other   teams.Team
	}{
		{firstService, firstRequest, firstTeam, secondTeam},
		{secondService, secondRequest, secondTeam, firstTeam},
	} {
		listed, err := fixture.service.ListTeams(context.Background(), fixture.request)
		if err != nil || len(listed) != 1 || listed[0].ID != fixture.own.ID {
			t.Fatalf("session %s listed %+v, err=%v; want only %s", fixture.request.Work.SessionID, listed, err, fixture.own.ID)
		}
		if _, err := fixture.service.GetTeam(context.Background(), fixture.request, fixture.own.ID); err != nil {
			t.Fatalf("own team %s was not queryable: %v", fixture.own.ID, err)
		}
		if _, err := fixture.service.GetTeam(context.Background(), fixture.request, fixture.other.ID); err == nil {
			t.Fatalf("session %s queried another session's team %s", fixture.request.Work.SessionID, fixture.other.ID)
		}
	}
}

func TestGoalTeamIsIsolatedByGoalAndWorkItem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team fixture")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	goalRoot := filepath.Join(root, "goal")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"goal-one", "goal-two"} {
		goal := coreGoal(id, goalRoot, session.ID)
		goal.Objective = "team scope fixture"
		if _, err := state.CreateGoal(context.Background(), goal); err != nil {
			t.Fatal(err)
		}
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-one", WorkItemID: "item-one"}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "goal-run", WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "team scope fixture"}); err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{ProjectRoot: root, Store: state}, activeRuns: map[string]string{"goal-run": session.ID}}
	request := agent.ExecutionRequest{RunID: "goal-run", Work: work}
	team, err := service.CreateTeam(context.Background(), request, "goal-research")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetTeam(context.Background(), request, team.ID); err != nil {
		t.Fatalf("matching goal work item could not query its team: %v", err)
	}
	for _, forged := range []agent.WorkRef{
		{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-one", WorkItemID: "item-two"},
		{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-two", WorkItemID: "item-one"},
	} {
		if _, err := service.GetTeam(context.Background(), agent.ExecutionRequest{RunID: "goal-run", Work: forged}, team.ID); err == nil {
			t.Fatalf("goal team was queryable from forged work scope: %+v", forged)
		}
	}
}

func TestTeamLeadMustBePersistedForSameWork(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-a")
	request.RunID = "unknown-run"
	if _, err := service.CreateTeam(context.Background(), request, "research"); err == nil {
		t.Fatal("unknown lead run created a team")
	}
	request.RunID = "run-a"
	request.TeamTurn = &agent.TeamTurnIdentity{TeamID: "team", MemberID: "member", TurnID: "turn"}
	if _, err := service.CreateTeam(context.Background(), request, "research"); err == nil {
		t.Fatal("member turn created a sibling team")
	}
}

func TestStopTeamMemberWithoutSchedulerFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-a")
	team, err := service.CreateTeam(context.Background(), request, "stop-check")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StopTeamMember(context.Background(), request.Work.SessionID, team.ID, "member-a"); err == nil || !strings.Contains(err.Error(), "scheduler is unavailable") {
		t.Fatalf("missing scheduler did not fail closed: %v", err)
	}
}

func TestTeamCoordinatorModeIsScopedToSessionAndCanBeDisabled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-a")
	other, err := sessionlog.Create(root, "other coordinator session")
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		response, err := service.handleTeamRequest(context.Background(), ClientMsg{Op: "team_coordinator", SessionID: request.Work.SessionID, CoordinatorOn: enabled})
		if err != nil || response.CoordinatorOn != enabled {
			t.Fatalf("coordinator mode response %+v, %v", response, err)
		}
		stored, err := teamCoordinatorModeEnabled(root, request.Work.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if stored != enabled {
			t.Fatalf("session coordinator mode=%v, want %v", stored, enabled)
		}
	}
	otherMode, err := teamCoordinatorModeEnabled(root, other.ID)
	if err != nil || otherMode {
		t.Fatalf("coordinator mode leaked to second session: %v, %v", otherMode, err)
	}
}

func TestTeamTaskServicePersistsDependencyGateAndRevision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "run-a")
	team, err := service.CreateTeam(context.Background(), request, "build")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "prepare", CreatedBy: "forged", Revision: 99, Status: teams.TaskCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if first.CreatedBy != teams.Lead || first.Revision != 1 || first.Status != teams.TaskPending {
		t.Fatalf("service did not own task identity and initial state: %+v", first)
	}
	second, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "finish", BlockedBy: []string{first.ID}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := teams.TaskInProgress
	if _, err = service.UpdateTeamTask(context.Background(), request, team.ID, second.ID, second.Revision, teams.TaskPatch{Status: &inProgress}); err == nil {
		t.Fatal("task with an incomplete dependency entered progress")
	}
	completed := teams.TaskCompleted
	first, err = service.UpdateTeamTask(context.Background(), request, team.ID, first.ID, first.Revision, teams.TaskPatch{Status: &completed})
	if err != nil {
		t.Fatal(err)
	}
	second, err = service.UpdateTeamTask(context.Background(), request, team.ID, second.ID, second.Revision, teams.TaskPatch{Status: &inProgress})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := service.ListTeamTasks(context.Background(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("unexpected durable dependency projection: %+v", listed)
	}
	var listedSecond teams.Task
	for _, task := range listed {
		if task.ID == second.ID {
			listedSecond = task
		}
	}
	if listedSecond.Status != teams.TaskInProgress || len(listedSecond.BlockedBy) != 1 || listedSecond.BlockedBy[0] != first.ID {
		t.Fatalf("unexpected durable dependency projection: %+v", listed)
	}
	if _, err = service.UpdateTeamTask(context.Background(), request, team.ID, second.ID, second.Revision-1, teams.TaskPatch{Title: stringPtr("stale")}); !errors.Is(err, teams.ErrRevisionConflict) {
		t.Fatalf("stale update = %v, want revision conflict", err)
	}
}

func stringPtr(value string) *string { return &value }

func TestTeamProtocolUsesServerOwnedScope(t *testing.T) {
	sessionID, runID := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	if err := validateClient(ClientMsg{Op: "team_create", SessionID: sessionID, RunID: runID, TeamName: "research"}); err != nil {
		t.Fatalf("valid team create request rejected: %v", err)
	}
	for _, request := range []ClientMsg{
		{Op: "team_create", SessionID: sessionID, TeamName: "research"},
		{Op: "team_create", SessionID: sessionID, RunID: runID, TeamName: "research", ProjectRoot: "/tmp/forged"},
		{Op: "team_get", SessionID: sessionID, RunID: runID},
	} {
		if err := validateClient(request); err == nil {
			t.Fatalf("invalid team request was accepted: %+v", request)
		}
	}
	if err := validateClient(ClientMsg{Op: "team_task_create", SessionID: sessionID, RunID: runID, TeamID: "team", TaskTitle: stringPtr("inspect")}); err != nil {
		t.Fatalf("valid task create request rejected: %v", err)
	}
	if err := validateClient(ClientMsg{Op: "team_task_update", SessionID: sessionID, RunID: runID, TeamID: "team", TaskID: "task", ExpectedRevision: 1, TaskStatus: stringPtr(string(teams.TaskCompleted))}); err != nil {
		t.Fatalf("valid task update request rejected: %v", err)
	}
	for _, request := range []ClientMsg{
		{Op: "team_task_create", SessionID: sessionID, RunID: runID, TeamID: "team", TaskTitle: stringPtr(" ")},
		{Op: "team_task_update", SessionID: sessionID, RunID: runID, TeamID: "team", TaskID: "task", TaskStatus: stringPtr("blocked"), ExpectedRevision: 1},
		{Op: "team_task_update", SessionID: sessionID, RunID: runID, TeamID: "team", TaskID: "task", TaskTitle: stringPtr("rename")},
	} {
		if err := validateClient(request); err == nil {
			t.Fatalf("invalid task request was accepted: %+v", request)
		}
	}
}
