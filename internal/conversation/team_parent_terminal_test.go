package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type terminalIndependentTeamChildRunner struct {
	started chan teamChildStart
	release chan struct{}
}

func (r *terminalIndependentTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- teamChildStart{input: input, ctx: ctx}
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed after parent"}
}

func TestTeamChildContinuesAfterParentRunCompletes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-completed")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
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
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "parent-completion")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.", OriginCallID: "spawn-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)

	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-terminal", RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("parent completion canceled accepted team child: %v", child.ctx.Err())
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("parent completion changed child member status to %s, want running", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn status after parent completion = %s, want succeeded", got)
	}
}

func TestTeamChildContinuesAfterParentRunFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-failed")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
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
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "parent-failure")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.", OriginCallID: "spawn-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)

	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-terminal", RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("parent failure canceled accepted team child: %v", child.ctx.Err())
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("parent failure changed child member status to %s, want running", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn status after parent failure = %s, want succeeded", got)
	}
}
