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

func TestTeamPlanApprovalResponseSameValueRetryIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "plan-approval-retry-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 2), release: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ToolSchemas = []llm.ToolSchema{{Name: "read_file"}, {Name: "team_plan_submit"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		for range 2 {
			select {
			case runner.release <- struct{}{}:
			default:
			}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "plan-approval-retry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "spawn-plan-approval-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial child turn=%+v, want member %s", first.TeamTurn, member.ID)
	}
	childRequest := agent.ExecutionRequest{RunID: first.ChildRunID, Work: request.Work, TeamTurn: first.TeamTurn}
	planResult, err := service.ExecuteTeamTool(t.Context(), childRequest, llm.ToolUse{
		ID: "submit-approval-retry-plan", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + team.ID + `","body":"Inspect the parser entry point."}`),
	})
	if err != nil || planResult.Status != agent.ToolSucceeded {
		t.Fatalf("plan submission=%+v err=%v", planResult, err)
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
	if pending.ID == "" || pending.Status != teams.RequestPending || pending.ResponderID != teams.Lead {
		t.Fatalf("pending plan request=%+v, want lead responder", pending)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberAwaitingPlan)

	feedback := "Approved after the inspection scope is clear."
	approved, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), feedback)
	if err != nil || approved.Status != teams.RequestApproved || approved.ResponderID != teams.Lead {
		t.Fatalf("lead approval=%+v err=%v", approved, err)
	}
	followUp := receiveTeamChildInput(t, runner.inputs)
	if followUp.TeamTurn == nil || followUp.TeamTurn.MemberID != member.ID || followUp.TeamTurn.TurnID == first.TeamTurn.TurnID || runner.childCount() != 2 {
		t.Fatalf("approval follow-up=%+v child count=%d", followUp.TeamTurn, runner.childCount())
	}
	beforeRetry, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEventCount := len(transcript.Events)
	beforeMemberRevision := beforeRetry.Members[member.ID].Revision
	beforeTeamRevision := beforeRetry.Teams[team.ID].Revision

	retried, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), feedback)
	if err != nil || !reflect.DeepEqual(retried, approved) {
		t.Fatalf("same-value approval retry=%+v err=%v, want original response %+v", retried, err, approved)
	}
	assertPlanResponseRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, beforeRetry, beforeEventCount, beforeMemberRevision, beforeTeamRevision, runner, 2)

	for _, conflict := range []struct {
		name     string
		decision string
		feedback string
	}{
		{name: "opposite decision", decision: string(teams.RequestRejected), feedback: feedback},
		{name: "different feedback", decision: string(teams.RequestApproved), feedback: "changed approval feedback"},
	} {
		t.Run(conflict.name, func(t *testing.T) {
			if _, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, conflict.decision, conflict.feedback); !errors.Is(err, teams.ErrRevisionConflict) {
				t.Fatalf("conflicting response error=%v, want revision conflict", err)
			}
			assertPlanResponseRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, beforeRetry, beforeEventCount, beforeMemberRevision, beforeTeamRevision, runner, 2)
		})
	}
}
