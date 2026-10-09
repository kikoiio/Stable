package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberCannotSubmitSecondUnresolvedPlan(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "single-pending-plan-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	role := agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3,
	}
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

	team, err := service.CreateTeam(t.Context(), request, "single-pending-plan")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.",
		PlanRequired: true, OriginCallID: "single-pending-plan-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	if child.TeamTurn == nil || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected running member turn: %+v", child.TeamTurn)
	}
	childRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: request.Work, TeamTurn: child.TeamTurn}
	first, err := service.SubmitTeamPlan(t.Context(), childRequest, team.ID, "Inspect the parser and report findings.")
	if err != nil || first.Status != teams.RequestPending || first.MemberID != member.ID {
		t.Fatalf("first plan submission = %+v, err=%v", first, err)
	}
	beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitTeamPlan(t.Context(), childRequest, team.ID, "A second plan must not replace the pending one."); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("second unresolved plan submission = %v, want ErrCapacity", err)
	}
	afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) {
		t.Fatalf("rejected second plan changed projection: before=%+v after=%+v", beforeProjection, afterProjection)
	}
	afterHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterHistory) != len(beforeHistory) {
		t.Fatalf("rejected second plan appended team facts: before=%d after=%d", len(beforeHistory), len(afterHistory))
	}
	afterSession, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSession.Events) != len(beforeSession.Events) || runner.childCount() != 1 {
		t.Fatalf("rejected second plan changed session events or started another child: events %d -> %d; children=%d", len(beforeSession.Events), len(afterSession.Events), runner.childCount())
	}
	if len(afterProjection.Requests) != 1 || afterProjection.Requests[first.ID].Body != first.Body {
		t.Fatalf("single pending plan was not preserved: %+v", afterProjection.Requests)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberAwaitingPlan)
}
