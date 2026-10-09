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

func TestStoppingTeamMemberLeavesSiblingTurnRunning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stop-sibling-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &cancelAwareTeamChildRunner{
		started: make(chan teamChildStart, 2), canceled: make(chan string, 2),
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
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "stop-sibling-isolation")
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
	startA := receiveTeamChildStart(t, runner.started)
	startB := receiveTeamChildStart(t, runner.started)
	starts := map[string]teamChildStart{
		startA.input.TeamTurn.MemberID: startA,
		startB.input.TeamTurn.MemberID: startB,
	}
	startA, okA := starts[memberA.ID]
	startB, okB := starts[memberB.ID]
	if !okA || !okB || startA.input.TeamTurn.TurnID == startB.input.TeamTurn.TurnID {
		t.Fatalf("children did not start as independent member turns: %+v", starts)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberRunning)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberRunning)

	stopping, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, memberA.ID)
	if err != nil || stopping.Status != teams.MemberStopping || stopping.TurnID != startA.input.TeamTurn.TurnID {
		t.Fatalf("stop A = %+v, %v; want only A's turn stopping", stopping, err)
	}
	select {
	case <-startA.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stopping member A did not cancel its child context")
	}
	select {
	case turnID := <-runner.canceled:
		if turnID != startA.input.TeamTurn.TurnID {
			t.Fatalf("first canceled turn=%s, want member A turn %s", turnID, startA.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("member A runner did not observe its cancellation")
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberStopped)
	select {
	case <-startB.ctx.Done():
		t.Fatal("stopping member A canceled sibling B")
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[startA.input.TeamTurn.TurnID].Status; got != string(agent.DelegationCanceled) {
		t.Fatalf("member A turn status=%s, want canceled", got)
	}
	if got := projection.Members[memberB.ID].Status; got != teams.MemberRunning {
		t.Fatalf("stopping member A changed sibling B status to %s", got)
	}
	if got := projection.Turns[startB.input.TeamTurn.TurnID].Status; got != "queued" {
		t.Fatalf("stopping member A changed sibling B turn status to %s", got)
	}

	if _, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, memberB.ID); err != nil {
		t.Fatalf("cleanup stop for sibling B: %v", err)
	}
	select {
	case turnID := <-runner.canceled:
		if turnID != startB.input.TeamTurn.TurnID {
			t.Fatalf("second canceled turn=%s, want member B turn %s", turnID, startB.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sibling B did not stop during cleanup")
	}
}
