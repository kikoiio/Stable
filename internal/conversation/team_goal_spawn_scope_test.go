package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

type goalSpawnScopeChildRunner struct{ started chan agent.ChildRunInput }

func (r *goalSpawnScopeChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "authorized owner spawn completed"}
}

// Sibling WorkItems and other Goals may each have a valid lead run, but
// neither can create a member under the owner's team identity.
func TestGoalTeamSpawnRejectsSiblingWorkItemAndGoalOwnerWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	otherGoalRoot := filepath.Join(root, "other-goal-root")
	for _, dir := range []string{root, goalRoot, otherGoalRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "goal team spawn scope")
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
	const goalID = "goal-spawn-scope"
	if _, err := state.CreateGoal(t.Context(), coreGoal(goalID, goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	const otherGoalID = "goal-spawn-other-scope"
	if _, err := state.CreateGoal(t.Context(), coreGoal(otherGoalID, otherGoalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-owner"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-sibling"}
	workC := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: otherGoalID, WorkItemID: "item-other-goal"}
	makeRequest := func(runID string, work agent.WorkRef) agent.ExecutionRequest {
		t.Helper()
		allowedRoot := goalRoot
		if work.GoalID == otherGoalID {
			allowedRoot = otherGoalRoot
		}
		request := appendGoalScopeRun(t, root, runID, work)
		request.PermissionBounds, err = json.Marshal(permission.Authority{
			RunID: runID, SessionID: session.ID, GoalID: work.GoalID, WorkItemID: work.WorkItemID, AllowedRoot: allowedRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		request.ProviderName, request.Model = "fixture", "model-v1"
		return request
	}
	requestA := makeRequest("goal-spawn-owner-run", workA)
	requestB := makeRequest("goal-spawn-sibling-run", workB)
	requestC := makeRequest("goal-spawn-other-owner-run", workC)
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state, ProviderName: "fixture", Model: "model-v1"},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID, requestC.RunID: session.ID},
	}
	team, err := service.CreateTeam(t.Context(), requestA, "spawn-owner-team")
	if err != nil {
		t.Fatal(err)
	}
	_, siblingScope, siblingActor, err := service.teamOperationScope(t.Context(), requestB)
	if err != nil || !siblingActor.Lead || siblingScope.WorkItemID != workB.WorkItemID || siblingScope.ProjectRoot != team.Scope.ProjectRoot {
		t.Fatalf("fixture sibling run is not a valid lead for its own WorkItem/root: scope=%+v actor=%+v err=%v", siblingScope, siblingActor, err)
	}
	_, otherGoalScope, otherGoalActor, err := service.teamOperationScope(t.Context(), requestC)
	if err != nil || !otherGoalActor.Lead || otherGoalScope.GoalID != otherGoalID || otherGoalScope.WorkItemID != workC.WorkItemID || otherGoalScope.ProjectRoot != otherGoalRoot {
		t.Fatalf("fixture other-Goal run is not a valid lead for its own WorkItem/root: scope=%+v actor=%+v err=%v", otherGoalScope, otherGoalActor, err)
	}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 1}
	runner := &goalSpawnScopeChildRunner{started: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})

	beforeProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SpawnTeamMember(t.Context(), requestB, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "sibling-reader", AgentName: role.Name,
		Instruction: "Try to join the other WorkItem's team.", OriginCallID: "sibling-workitem-spawn",
	}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("sibling WorkItem member spawn error=%v, want ErrPermission", err)
	}
	if _, err := service.SpawnTeamMember(t.Context(), requestC, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "other-goal-reader", AgentName: role.Name,
		Instruction: "Try to join a different Goal's team.", OriginCallID: "other-goal-spawn",
	}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("other Goal member spawn error=%v, want ErrPermission", err)
	}
	afterProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeProjection, afterProjection) || !reflect.DeepEqual(beforeHistory, afterHistory) || !reflect.DeepEqual(beforeSession.Events, afterSession.Events) {
		t.Fatal("rejected sibling WorkItem or other Goal spawn changed team projection, history, or session events")
	}
	service.teamScheduler.mu.Lock()
	if len(service.teamScheduler.active) != 0 || len(service.teamScheduler.ready) != 0 || len(service.teamScheduler.waiting) != 0 || len(service.teamScheduler.grants) != 0 {
		service.teamScheduler.mu.Unlock()
		t.Fatalf("rejected spawn mutated scheduler: active=%d ready=%d waiting=%d grants=%d", len(service.teamScheduler.active), len(service.teamScheduler.ready), len(service.teamScheduler.waiting), len(service.teamScheduler.grants))
	}
	service.teamScheduler.mu.Unlock()
	select {
	case input := <-runner.started:
		t.Fatalf("rejected sibling spawn started child turn: %+v", input.TeamTurn)
	default:
	}

	// The exact WorkItem owner retains the ability to use the same team.
	member, err := service.SpawnTeamMember(t.Context(), requestA, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "owner-reader", AgentName: role.Name,
		Instruction: "Inspect the owner WorkItem.", OriginCallID: "owner-workitem-spawn",
	})
	if err != nil || member.TeamID != team.ID {
		t.Fatalf("owner WorkItem spawn = %+v, %v; want success", member, err)
	}
	select {
	case input := <-runner.started:
		if input.TeamTurn == nil || input.TeamTurn.TeamID != team.ID || input.TeamTurn.MemberID != member.ID || input.ParentRunID != requestA.RunID {
			t.Fatalf("owner spawn started wrong child: %+v", input)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authorized owner spawn did not reach child runner")
	}
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberIdle)
}
