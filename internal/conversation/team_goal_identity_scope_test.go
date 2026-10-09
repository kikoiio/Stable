package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamsWithSameNameRejectCrossSessionStop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRootA := filepath.Join(root, "goal-a-root")
	goalRootB := filepath.Join(root, "goal-b-root")
	for _, path := range []string{goalRootA, goalRootB} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sessionA, err := sessionlog.Create(root, "goal identity A")
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := sessionlog.Create(root, "goal identity B")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	goalA := coreGoal("goal-identity-a", goalRootA, sessionA.ID)
	goalB := coreGoal("goal-identity-b", goalRootB, sessionB.ID)
	if _, err := state.CreateGoal(context.Background(), goalA); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateGoal(context.Background(), goalB); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionA.ID, GoalID: goalA.ID, WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionB.ID, GoalID: goalB.ID, WorkItemID: "item-b"}
	const runA = "goal-identity-run-a"
	const runB = "goal-identity-run-b"
	for _, run := range []struct {
		id      string
		session string
		work    agent.WorkRef
	}{{runA, sessionA.ID, workA}, {runB, sessionB.ID, workB}} {
		if _, err := sessionlog.Append(root, run.session, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: run.id, WorkKind: string(run.work.Kind), GoalID: run.work.GoalID, WorkItemID: run.work.WorkItemID, Intent: "same team name identity fixture",
		}); err != nil {
			t.Fatal(err)
		}
	}
	requestA := agent.ExecutionRequest{RunID: runA, Work: workA}
	requestA.PermissionBounds, err = json.Marshal(permission.Authority{RunID: runA, SessionID: sessionA.ID, GoalID: workA.GoalID, WorkItemID: workA.WorkItemID, AllowedRoot: goalRootA})
	if err != nil {
		t.Fatal(err)
	}
	requestB := agent.ExecutionRequest{RunID: runB, Work: workB}
	requestB.PermissionBounds, err = json.Marshal(permission.Authority{RunID: runB, SessionID: sessionB.ID, GoalID: workB.GoalID, WorkItemID: workB.WorkItemID, AllowedRoot: goalRootB})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		deps:           Deps{ProjectRoot: root, Store: state, ProviderName: "fixture", Model: "model-v1"},
		activeRuns:     map[string]string{runA: sessionA.ID, runB: sessionB.ID},
		activeRequests: map[string]agent.ExecutionRequest{runA: requestA, runB: requestB},
	}
	teamA, err := service.CreateTeam(t.Context(), requestA, "shared-display-name")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(t.Context(), requestB, "shared-display-name")
	if err != nil {
		t.Fatalf("same display name in a different Goal/session scope was rejected: %v", err)
	}
	if teamA.ID == teamB.ID || teamA.Name != teamB.Name || teamA.Scope.SessionID == teamB.Scope.SessionID || teamA.Scope.GoalID == teamB.Scope.GoalID {
		t.Fatalf("same-name Goal teams did not retain independent identities/scopes: A=%+v B=%+v", teamA, teamB)
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
	member, err := service.SpawnTeamMember(t.Context(), requestA, TeamMemberSpawnRequest{
		TeamID: teamA.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "goal-identity-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveTeamChildInput(t, runner.inputs)
	before, err := sessionlog.TeamHistory(root, sessionA.ID, teamA.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StopTeamMember(t.Context(), sessionB.ID, teamA.ID, member.ID); err == nil {
		t.Fatal("different session/Goal user stopped the target Goal member")
	}
	after, err := sessionlog.TeamHistory(root, sessionA.ID, teamA.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("rejected cross-scope stop appended facts: before=%d after=%d", len(before), len(after))
	}
	projection, err := sessionlog.ReplayTeams(root, sessionA.ID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID]; got.Status != teams.MemberRunning || got.TurnID != input.TeamTurn.TurnID {
		t.Fatalf("rejected cross-scope stop changed target member: %+v", got)
	}
	if len(projection.Requests) != 0 {
		t.Fatalf("rejected cross-scope stop created requests: %+v", projection.Requests)
	}

	if _, err := service.StopTeamMember(t.Context(), sessionA.ID, teamA.ID, member.ID); err != nil {
		t.Fatalf("authorized Goal scope could not stop its member: %v", err)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, sessionA.ID, teamA.ID, member.ID, teams.MemberStopped)
}

func TestCreateTeamRejectsSessionCapacityWithoutPersistingFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-capacity-lead")
	teamsBefore := make([]teams.Team, 0, teams.MaxSessionTeams)
	for i := 0; i < teams.MaxSessionTeams; i++ {
		created, err := service.CreateTeam(t.Context(), request, []string{"capacity-one", "capacity-two"}[i])
		if err != nil {
			t.Fatalf("create team %d: %v", i+1, err)
		}
		teamsBefore = append(teamsBefore, created)
	}
	if _, err := service.CreateTeam(t.Context(), request, "capacity-overflow"); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("create beyond session team limit = %v, want ErrCapacity", err)
	}
	listed, err := service.ListTeams(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(teamsBefore) {
		t.Fatalf("rejected team creation changed visible team state: got %d teams, want %d", len(listed), len(teamsBefore))
	}
	for _, team := range teamsBefore {
		history, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
		if err != nil {
			t.Fatal(err)
		}
		if len(history) != 1 {
			t.Fatalf("existing team %s history grew after rejected create: %d facts", team.ID, len(history))
		}
	}
}

func TestSpawnTeamMemberRejectsDuplicateNameWithoutPersistingFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "duplicate-member-lead")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	team, err := service.CreateTeam(t.Context(), request, "duplicate-member-team")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, team.ID, "existing-reader", "reader")
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
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
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	if _, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: " READER ", AgentName: role.Name, Instruction: "This duplicate must be rejected.", OriginCallID: "duplicate-member-spawn",
	}); err == nil {
		t.Fatal("duplicate normalized member name was accepted")
	}
	if got := runner.childCount(); got != 0 {
		t.Fatalf("rejected duplicate member started %d child runs", got)
	}
	after, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Teams[team.ID].Revision != before.Teams[team.ID].Revision || len(after.Members) != len(before.Members) {
		t.Fatalf("rejected duplicate changed team projection: before=%+v after=%+v", before, after)
	}
	if got := after.Members["existing-reader"]; got.Name != "reader" || got.Status != teams.MemberCreated || got.Revision != before.Members["existing-reader"].Revision {
		t.Fatalf("rejected duplicate changed existing member: %+v", got)
	}
	historyAfter, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfter) != len(historyBefore) {
		t.Fatalf("rejected duplicate appended durable facts: before=%d after=%d", len(historyBefore), len(historyAfter))
	}
}

func TestSpawnTeamMemberRejectsTeamCapacityWithoutPersistingFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "member-capacity-lead")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	team, err := service.CreateTeam(t.Context(), request, "member-capacity-team")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < teams.MaxTeamMembers; i++ {
		addTeamMessageMember(t, service, request, team.ID, fmt.Sprintf("capacity-member-%d", i), fmt.Sprintf("reader-%d", i))
	}
	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
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
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	if _, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "overflow-reader", AgentName: role.Name, Instruction: "Capacity must reject this member.", OriginCallID: "team-member-capacity-overflow",
	}); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("spawn beyond team member limit = %v, want ErrCapacity", err)
	}
	if got := runner.childCount(); got != 0 {
		t.Fatalf("rejected over-capacity member started %d child runs", got)
	}
	after, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Teams[team.ID].Revision != before.Teams[team.ID].Revision || len(after.Members) != len(before.Members) {
		t.Fatalf("rejected over-capacity member changed projection: before=%+v after=%+v", before, after)
	}
	for id, member := range before.Members {
		if !reflect.DeepEqual(after.Members[id], member) {
			t.Fatalf("rejected over-capacity member changed existing member %s: before=%+v after=%+v", id, member, after.Members[id])
		}
	}
	historyAfter, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfter) != len(historyBefore) {
		t.Fatalf("rejected over-capacity member appended facts: before=%d after=%d", len(historyBefore), len(historyAfter))
	}
}
