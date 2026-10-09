package conversation

import (
	"context"
	"encoding/json"
	"errors"
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

func TestApprovedTeamPlanMemberStateGapRecoversAndRetriesOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "plan-approval-gap-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 3), release: make(chan struct{}, 3)}
	newPool := func() *agent.PoolDelegator {
		pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
		if err != nil {
			t.Fatal(err)
		}
		return pool
	}
	pool := newPool()
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ToolSchemas = []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}, {Name: "command"}, {Name: "team_plan_submit"}, {Name: "team_member_spawn"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	closeService := func(scheduler *teamScheduler, childPool *agent.PoolDelegator) {
		if scheduler != nil {
			scheduler.close()
		}
		if childPool != nil {
			childPool.Close()
		}
	}
	releaseBlockedChildren := func() {
		for range 3 {
			select {
			case runner.release <- struct{}{}:
			default:
			}
		}
	}
	serviceClosed := false
	t.Cleanup(func() {
		if !serviceClosed {
			releaseBlockedChildren()
			closeService(service.teamScheduler, pool)
		}
	})

	team, err := service.CreateTeam(t.Context(), request, "plan-approval-gap")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "spawn-plan-gap",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial child turn=%+v, want member %s", first.TeamTurn, member.ID)
	}
	childRequest := agent.ExecutionRequest{RunID: first.ChildRunID, Work: request.Work, TeamTurn: first.TeamTurn}
	result, err := service.ExecuteTeamTool(t.Context(), childRequest, llm.ToolUse{
		ID: "submit-plan-gap", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + team.ID + `","body":"Inspect the parser entry point."}`),
	})
	if err != nil || result.Status != agent.ToolSucceeded {
		t.Fatalf("plan submission=%+v err=%v", result, err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pending teams.Request
	for _, candidate := range projection.Requests {
		if candidate.MemberID == member.ID && candidate.Type == teams.RequestPlan {
			pending = candidate
		}
	}
	if pending.ID == "" || pending.Status != teams.RequestPending {
		t.Fatalf("plan request=%+v, want pending request", pending)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberAwaitingPlan)

	appendFailure := errors.New("injected plan approval member-state append failure")
	service.teamMemberStateAppender = func(string, teams.Team, string, string, teams.Member) error {
		return appendFailure
	}
	feedback := "Approved for the read-only inspection."
	if _, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), feedback); !errors.Is(err, appendFailure) {
		t.Fatalf("approval with injected member-state failure=%v, want %v", err, appendFailure)
	}
	service.teamMemberStateAppender = nil
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[pending.ID].Status; got != teams.RequestApproved {
		t.Fatalf("persisted request status=%s, want approved", got)
	}
	if got := projection.Members[member.ID]; got.PlanApproved || got.Status != teams.MemberAwaitingPlan {
		t.Fatalf("member after failed state append=%+v, want unapproved/awaiting-plan", got)
	}

	closeService(service.teamScheduler, pool)
	serviceClosed = true
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("recover plan approval gap: %v", err)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("repeat recovery plan approval gap: %v", err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID]; !got.PlanApproved || got.Status == teams.MemberAwaitingPlan {
		t.Fatalf("member after recovery=%+v, want approved and resumable", got)
	}

	restarted := &Service{
		deps: service.deps, lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID},
		activeRequests: map[string]agent.ExecutionRequest{request.RunID: request},
	}
	restartedPool := newPool()
	restarted.deps.Delegator = restartedPool
	restarted.teamScheduler = newTeamScheduler(restarted)
	t.Cleanup(func() {
		releaseBlockedChildren()
		closeService(restarted.teamScheduler, restartedPool)
	})

	retried, err := restarted.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), feedback)
	if err != nil || retried.Status != teams.RequestApproved {
		t.Fatalf("same-value approval retry after recovery=%+v err=%v", retried, err)
	}
	followUp := receiveTeamChildInput(t, runner.inputs)
	if followUp.TeamTurn == nil || followUp.TeamTurn.MemberID != member.ID || followUp.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("recovered approval follow-up=%+v, want a fresh member turn", followUp.TeamTurn)
	}
	assertTeamPlanToolSchemas(t, followUp.ToolSchemas)
	if runner.childCount() != 2 {
		t.Fatalf("approval follow-up child count=%d, want exactly two total", runner.childCount())
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)

	if _, err := restarted.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), feedback); err != nil {
		t.Fatalf("same-value approval retry after follow-up=%v", err)
	}
	select {
	case duplicate := <-runner.inputs:
		t.Fatalf("duplicate approved-plan follow-up started: %+v", duplicate.TeamTurn)
	case <-time.After(150 * time.Millisecond):
	}
	if runner.childCount() != 2 {
		t.Fatalf("duplicate approval retry started %d child turns, want exactly two total", runner.childCount())
	}
}
