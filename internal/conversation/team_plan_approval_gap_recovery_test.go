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
	restartedClosed := false
	t.Cleanup(func() {
		if !restartedClosed {
			releaseBlockedChildren()
			closeService(restarted.teamScheduler, restartedPool)
		}
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

	// Exercise the other recovery order: after approval recovery, the lead may
	// explicitly resume the member before retrying the old approval response.
	// That turn must carry the approved request ID so the later retry cannot
	// create a second follow-up.
	secondMember, err := restarted.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-two", AgentName: role.Name, Instruction: "Inspect the second area.", PlanRequired: true, OriginCallID: "spawn-plan-gap-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInitial := receiveTeamChildInput(t, runner.inputs)
	if secondInitial.TeamTurn == nil || secondInitial.TeamTurn.MemberID != secondMember.ID {
		t.Fatalf("second initial turn=%+v, want member %s", secondInitial.TeamTurn, secondMember.ID)
	}
	secondChildRequest := agent.ExecutionRequest{RunID: secondInitial.ChildRunID, Work: request.Work, TeamTurn: secondInitial.TeamTurn}
	result, err = restarted.ExecuteTeamTool(t.Context(), secondChildRequest, llm.ToolUse{
		ID: "submit-plan-gap-second", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + team.ID + `","body":"Inspect the second parser entry point."}`),
	})
	if err != nil || result.Status != agent.ToolSucceeded {
		t.Fatalf("second plan submission=%+v err=%v", result, err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var secondPending teams.Request
	for _, candidate := range projection.Requests {
		if candidate.MemberID == secondMember.ID && candidate.Type == teams.RequestPlan {
			secondPending = candidate
		}
	}
	if secondPending.ID == "" || secondPending.Status != teams.RequestPending {
		t.Fatalf("second plan request=%+v, want pending request", secondPending)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, secondMember.ID, teams.MemberAwaitingPlan)
	restarted.teamMemberStateAppender = func(string, teams.Team, string, string, teams.Member) error { return appendFailure }
	if _, err := restarted.RespondTeamRequest(t.Context(), request, team.ID, secondPending.ID, secondPending.Revision, string(teams.RequestApproved), feedback); !errors.Is(err, appendFailure) {
		t.Fatalf("second approval with injected member-state failure=%v, want %v", err, appendFailure)
	}
	restarted.teamMemberStateAppender = nil
	closeService(restarted.teamScheduler, restartedPool)
	restartedClosed = true
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("recover second plan approval gap: %v", err)
	}

	resumed := &Service{
		deps: restarted.deps, lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID},
		activeRequests: map[string]agent.ExecutionRequest{request.RunID: request},
	}
	resumedPool := newPool()
	resumed.deps.Delegator = resumedPool
	resumed.teamScheduler = newTeamScheduler(resumed)
	t.Cleanup(func() {
		releaseBlockedChildren()
		closeService(resumed.teamScheduler, resumedPool)
	})
	if _, err := resumed.ResumeTeamMember(t.Context(), request, team.ID, secondMember.ID, "explicit-resume-after-plan-recovery"); err != nil {
		t.Fatalf("explicit resume after approval recovery=%v", err)
	}
	secondFollowUp := receiveTeamChildInput(t, runner.inputs)
	if secondFollowUp.TeamTurn == nil || secondFollowUp.TeamTurn.MemberID != secondMember.ID || secondFollowUp.TeamTurn.TurnID == secondInitial.TeamTurn.TurnID {
		t.Fatalf("explicit recovered turn=%+v, want a fresh second-member turn", secondFollowUp.TeamTurn)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[secondFollowUp.TeamTurn.TurnID].PlanRequestID; got != secondPending.ID {
		t.Fatalf("explicit recovered turn plan_request_id=%q, want %q", got, secondPending.ID)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, secondMember.ID, teams.MemberIdle)
	if _, err := resumed.RespondTeamRequest(t.Context(), request, team.ID, secondPending.ID, secondPending.Revision, string(teams.RequestApproved), feedback); err != nil {
		t.Fatalf("same-value approval retry after explicit recovered turn=%v", err)
	}
	select {
	case duplicate := <-runner.inputs:
		t.Fatalf("explicit resume was duplicated by approval retry: %+v", duplicate.TeamTurn)
	case <-time.After(150 * time.Millisecond):
	}
	if runner.childCount() != 4 {
		t.Fatalf("approval recovery and explicit resume started %d child turns, want four total", runner.childCount())
	}
}

func TestLatestUnconsumedApprovedTeamPlanRequestUsesDurableOrder(t *testing.T) {
	const teamID, memberID = "team-plan-order", "member-plan-order"
	approved := func(id string) sessionlog.Event {
		return sessionlog.Event{
			Type: sessionlog.EventTeam,
			Data: sessionlog.TeamEvent{
				TeamID: teamID, Kind: sessionlog.TeamRequestResponded,
				Request: &teams.Request{ID: id, TeamID: teamID, MemberID: memberID, Type: teams.RequestPlan, Status: teams.RequestApproved},
			},
		}
	}
	projection := sessionlog.TeamProjection{
		Turns: map[string]sessionlog.TurnFact{
			"turn-old":    {ID: "turn-old", MemberID: memberID, PlanRequestID: "request-old", Status: "succeeded"},
			"turn-newest": {ID: "turn-newest", MemberID: memberID, PlanRequestID: "request-newest", Status: "interrupted"},
		},
		Requests: map[string]teams.Request{
			"request-old":    {ID: "request-old", TeamID: teamID, MemberID: memberID, Type: teams.RequestPlan, Status: teams.RequestApproved},
			"request-middle": {ID: "request-middle", TeamID: teamID, MemberID: memberID, Type: teams.RequestPlan, Status: teams.RequestApproved},
			"request-newest": {ID: "request-newest", TeamID: teamID, MemberID: memberID, Type: teams.RequestPlan, Status: teams.RequestApproved},
		},
	}
	events := []sessionlog.Event{approved("request-old"), approved("request-middle"), approved("request-newest")}
	if got := latestUnconsumedApprovedTeamPlanRequestID(events, projection, teamID, memberID); got != "request-middle" {
		t.Fatalf("latest unconsumed approved request=%q, want request-middle", got)
	}
}
