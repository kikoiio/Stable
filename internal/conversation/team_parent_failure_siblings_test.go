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

type gatedParentFailureSiblingsRunner struct {
	started chan teamChildStart
	release chan struct{}
}

func (r *gatedParentFailureSiblingsRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- teamChildStart{input: input, ctx: ctx}
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed after parent failure"}
}

func TestTeamSiblingTurnsContinueAfterParentRunFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-failed-siblings")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedParentFailureSiblingsRunner{started: make(chan teamChildStart, 2), release: make(chan struct{}, 2)}
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
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "parent-failure-siblings")
	if err != nil {
		t.Fatal(err)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-a", AgentName: role.Name, Instruction: "Inspect area A.", OriginCallID: "spawn-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberB, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-b", AgentName: role.Name, Instruction: "Inspect area B.", OriginCallID: "spawn-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]teamChildStart{}
	for range 2 {
		child := receiveTeamChildStart(t, runner.started)
		starts[child.input.TeamTurn.MemberID] = child
	}
	childA, okA := starts[memberA.ID]
	childB, okB := starts[memberB.ID]
	if !okA || !okB || childA.input.TeamTurn.TurnID == childB.input.TeamTurn.TurnID {
		t.Fatalf("members did not start independent sibling turns: %+v", starts)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberRunning)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberRunning)

	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-terminal", RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	for memberID, child := range starts {
		select {
		case <-child.ctx.Done():
			t.Fatalf("parent failure canceled sibling %s: %v", memberID, child.ctx.Err())
		default:
		}
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []teams.Member{memberA, memberB} {
		if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
			t.Fatalf("parent failure changed member %s status to %s, want running", member.ID, got)
		}
	}

	runner.release <- struct{}{}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range starts {
		if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
			t.Fatalf("sibling turn %s after parent failure = %s, want succeeded", child.input.TeamTurn.TurnID, got)
		}
	}
}
