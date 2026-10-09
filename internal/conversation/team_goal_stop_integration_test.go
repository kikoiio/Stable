package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamMemberStopUsesPersistedWorkItemScopeAndWaitsForExit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team stop fixture")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err := state.CreateGoal(context.Background(), coreGoal("goal-stop", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-stop", WorkItemID: "item-stop"}
	const parentRunID = "goal-stop-parent"
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: parentRunID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal team stop fixture",
	}); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: parentRunID, Work: work}
	request.PermissionBounds, err = json.Marshal(permission.Authority{
		RunID: parentRunID, SessionID: session.ID, GoalID: work.GoalID, WorkItemID: work.WorkItemID, AllowedRoot: goalRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		deps:           Deps{ProjectRoot: root, Store: state},
		activeRuns:     map[string]string{parentRunID: session.ID},
		activeRequests: map[string]agent.ExecutionRequest{parentRunID: request},
	}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "goal-stop")
	if err != nil {
		t.Fatal(err)
	}
	if team.Scope.WorkKind != string(agent.WorkGoal) || team.Scope.GoalID != work.GoalID || team.Scope.WorkItemID != work.WorkItemID {
		t.Fatalf("team lost its exact Goal/WorkItem scope: %+v", team.Scope)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "goal-stop-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveTeamChildInput(t, runner.inputs)
	if input.TeamTurn == nil || input.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected child turn: %+v", input.TeamTurn)
	}

	userRequest, err := service.teamUserRequest(t.Context(), session.ID, team.ID)
	if err != nil || userRequest.Work != work || !userRequest.TeamUser {
		t.Fatalf("user stop request was not rebuilt from persisted Goal/WorkItem scope: %+v, err=%v", userRequest, err)
	}
	stopResponse, err := service.handleTeamRequest(t.Context(), ClientMsg{
		Op: "team_member_stop", SessionID: session.ID, TeamID: team.ID, TeamMemberID: member.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopResponse.TeamMember == nil {
		t.Fatalf("team_member_stop response omitted member: %+v", stopResponse)
	}
	stopping := *stopResponse.TeamMember
	if stopping.Status != teams.MemberStopping || stopping.TurnID != input.TeamTurn.TurnID {
		t.Fatalf("stop returned member %+v; want stopping turn %s", stopping, input.TeamTurn.TurnID)
	}
	assertTeamStopFacts(t, root, session.ID, team.ID, member.ID, input.TeamTurn.TurnID, false)
	if got := runner.childCount(); got != 1 {
		t.Fatalf("stop started another child: count=%d", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberStopped)
	assertTeamStopFacts(t, root, session.ID, team.ID, member.ID, input.TeamTurn.TurnID, true)
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Teams[team.ID].Scope; got.WorkKind != string(agent.WorkGoal) || got.GoalID != work.GoalID || got.WorkItemID != work.WorkItemID {
		t.Fatalf("stopped team replay changed Goal/WorkItem scope: %+v", got)
	}
	if service.activeRuns[parentRunID] != session.ID || request.RunID != parentRunID {
		t.Fatalf("team stop replaced parent run identity: active=%q request=%q", service.activeRuns[parentRunID], request.RunID)
	}
}
