package conversation

import (
	"context"
	"encoding/json"
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
	"stable/internal/teams"
)

type teamScopedStopIsolationRunner struct {
	started  chan teamChildStart
	canceled chan string
	release  map[string]chan struct{}
}

func (r *teamScopedStopIsolationRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- teamChildStart{input: input, ctx: ctx}
	select {
	case <-ctx.Done():
		r.canceled <- input.TeamTurn.TurnID
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	case <-r.release[input.TeamTurn.TeamID]:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed after other team stopped"}
	}
}

func TestStoppingMemberInOneTeamLeavesOtherTeamTurnRunning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stop-cross-team-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	teamBID := ""
	runner := &teamScopedStopIsolationRunner{
		started:  make(chan teamChildStart, 2),
		canceled: make(chan string, 2),
		release:  map[string]chan struct{}{},
	}
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
		if teamBID != "" {
			select {
			case runner.release[teamBID] <- struct{}{}:
			default:
			}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	teamA, err := service.CreateTeam(t.Context(), request, "stop-scope-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := service.CreateTeam(t.Context(), request, "stop-scope-b")
	if err != nil {
		t.Fatal(err)
	}
	teamBID = teamB.ID
	runner.release[teamA.ID] = make(chan struct{}, 1)
	runner.release[teamB.ID] = make(chan struct{}, 1)
	memberA, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: teamA.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect team A.", OriginCallID: "spawn-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberB, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: teamB.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect team B.", OriginCallID: "spawn-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]teamChildStart{}
	for range 2 {
		start := receiveTeamChildStart(t, runner.started)
		starts[start.input.TeamTurn.MemberID] = start
	}
	startA, okA := starts[memberA.ID]
	startB, okB := starts[memberB.ID]
	if !okA || !okB || startA.input.TeamTurn.TeamID != teamA.ID || startB.input.TeamTurn.TeamID != teamB.ID {
		t.Fatalf("children did not start in their expected teams: %+v", starts)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, teamA.ID, memberA.ID, teams.MemberRunning)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, teamB.ID, memberB.ID, teams.MemberRunning)
	beforeB, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamB.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}

	stopping, err := service.StopTeamMember(t.Context(), request.Work.SessionID, teamA.ID, memberA.ID)
	if err != nil || stopping.Status != teams.MemberStopping || stopping.TurnID != startA.input.TeamTurn.TurnID {
		t.Fatalf("stop team A member = %+v, %v; want only its turn stopping", stopping, err)
	}
	select {
	case <-startA.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stopping team A member did not cancel its child context")
	}
	select {
	case turnID := <-runner.canceled:
		if turnID != startA.input.TeamTurn.TurnID {
			t.Fatalf("canceled turn=%s, want team A turn %s", turnID, startA.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("team A runner did not observe cancellation")
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, teamA.ID, memberA.ID, teams.MemberStopped)
	projectionA, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectionA.Turns[startA.input.TeamTurn.TurnID].Status; got != string(agent.DelegationCanceled) {
		t.Fatalf("team A turn terminal status = %s, want canceled", got)
	}
	select {
	case <-startB.ctx.Done():
		t.Fatal("stopping team A member canceled team B child")
	default:
	}

	projectionB, err := sessionlog.ReplayTeams(root, request.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectionB.Members[memberB.ID].Status; got != teams.MemberRunning {
		t.Fatalf("stopping team A changed team B member status to %s", got)
	}
	if got := projectionB.Turns[startB.input.TeamTurn.TurnID].Status; got != "queued" {
		t.Fatalf("stopping team A changed team B turn status to %s", got)
	}
	afterB, err := sessionlog.TeamHistory(root, request.Work.SessionID, teamB.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterB) != len(beforeB) {
		t.Fatalf("stopping team A appended team B history: before=%d after=%d", len(beforeB), len(afterB))
	}
	for i := range beforeB {
		if beforeB[i].Seq != afterB[i].Seq || beforeB[i].Type != afterB[i].Type || !reflect.DeepEqual(beforeB[i].Data, afterB[i].Data) {
			t.Fatalf("stopping team A changed team B history at index %d: before=%+v after=%+v", i, beforeB[i], afterB[i])
		}
	}

	runner.release[teamB.ID] <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, teamB.ID, memberB.ID, teams.MemberIdle)
	projectionB, err = sessionlog.ReplayTeams(root, request.Work.SessionID, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projectionB.Turns[startB.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("team B turn after release = %s, want succeeded", got)
	}
}
