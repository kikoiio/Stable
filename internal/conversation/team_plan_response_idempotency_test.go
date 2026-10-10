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

func TestTeamPlanResponseSameValueRetryIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "plan-response-idempotency-parent")
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

	team, err := service.CreateTeam(t.Context(), request, "response-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "spawn-idempotency",
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
		ID: "submit-idempotent-plan", Name: "team_plan_submit",
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

	feedback := "Revise the report to name the parser entry point."
	rejected, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestRejected), feedback)
	if err != nil || rejected.Status != teams.RequestRejected || rejected.ResponderID != teams.Lead {
		t.Fatalf("lead rejection=%+v err=%v", rejected, err)
	}
	followUp := receiveTeamChildInput(t, runner.inputs)
	if followUp.TeamTurn == nil || followUp.TeamTurn.MemberID != member.ID || followUp.TeamTurn.TurnID == first.TeamTurn.TurnID || runner.childCount() != 2 {
		t.Fatalf("rejection follow-up=%+v child count=%d", followUp.TeamTurn, runner.childCount())
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

	retried, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestRejected), feedback)
	if err != nil || !reflect.DeepEqual(retried, rejected) {
		t.Fatalf("same-value retry=%+v err=%v, want original response %+v", retried, err, rejected)
	}
	assertPlanResponseRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, beforeRetry, beforeEventCount, beforeMemberRevision, beforeTeamRevision, runner, 2)

	for _, conflict := range []struct {
		name     string
		decision string
		feedback string
	}{
		{name: "opposite terminal decision", decision: string(teams.RequestApproved), feedback: feedback},
		{name: "different feedback", decision: string(teams.RequestRejected), feedback: "changed response feedback"},
	} {
		t.Run(conflict.name, func(t *testing.T) {
			if _, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, conflict.decision, conflict.feedback); !errors.Is(err, teams.ErrRevisionConflict) {
				t.Fatalf("conflicting response error=%v, want revision conflict", err)
			}
			assertPlanResponseRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, beforeRetry, beforeEventCount, beforeMemberRevision, beforeTeamRevision, runner, 2)
		})
	}

	followUpRequest := agent.ExecutionRequest{RunID: followUp.ChildRunID, Work: followUp.Work, TeamTurn: followUp.TeamTurn}
	if _, err := service.RespondTeamRequest(t.Context(), followUpRequest, team.ID, pending.ID, rejected.Revision, string(teams.RequestRejected), feedback); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("member retrying lead plan response error=%v, want permission error", err)
	}
	assertPlanResponseRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, beforeRetry, beforeEventCount, beforeMemberRevision, beforeTeamRevision, runner, 2)
}

func TestTeamShutdownDeferredResponseSameValueRetryIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "shutdown-defer-idempotency-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
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
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
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

	team, err := service.CreateTeam(t.Context(), request, "shutdown-defer-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "spawn-defer-idempotency",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	childRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: child.Work, TeamTurn: child.TeamTurn}
	shutdown, err := service.RequestTeamShutdown(t.Context(), request, team.ID, member.ID)
	if err != nil || shutdown.Status != teams.RequestPending {
		t.Fatalf("busy shutdown request=%+v err=%v", shutdown, err)
	}
	feedback := "Finish the current inspection before stopping."
	deferred, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), feedback)
	if err != nil || deferred.Status != teams.RequestDeferred || deferred.Revision != shutdown.Revision+1 {
		t.Fatalf("member defer=%+v err=%v", deferred, err)
	}

	before, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)
	memberRevision := before.Members[member.ID].Revision
	teamRevision := before.Teams[team.ID].Revision
	if before.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("defer changed active member state: %+v", before.Members[member.ID])
	}

	retried, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), feedback)
	if err != nil || !reflect.DeepEqual(retried, deferred) {
		t.Fatalf("same-value deferred retry=%+v err=%v, want original response %+v", retried, err, deferred)
	}
	assertShutdownDeferredRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, before, eventCount, memberRevision, teamRevision, runner)

	if _, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), "changed feedback"); !errors.Is(err, teams.ErrRevisionConflict) {
		t.Fatalf("conflicting deferred retry error=%v, want revision conflict", err)
	}
	assertShutdownDeferredRetryUnchanged(t, root, request.Work.SessionID, team.ID, member.ID, before, eventCount, memberRevision, teamRevision, runner)
}

func assertShutdownDeferredRetryUnchanged(t *testing.T, root, sessionID, teamID, memberID string, before sessionlog.TeamProjection, eventCount int, memberRevision, teamRevision uint64, runner *gatedTeamChildRunner) {
	t.Helper()
	after, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || after.Members[memberID].Revision != memberRevision || after.Teams[teamID].Revision != teamRevision {
		t.Fatalf("deferred retry changed team/member/request projection: before=%+v after=%+v", before, after)
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("deferred retry appended facts: before=%d after=%d", eventCount, len(transcript.Events))
	}
	if got := runner.childCount(); got != 1 {
		t.Fatalf("deferred retry started another child: count=%d want 1", got)
	}
}

func assertPlanResponseRetryUnchanged(t *testing.T, root, sessionID, teamID, memberID string, before sessionlog.TeamProjection, eventCount int, memberRevision, teamRevision uint64, runner *gatedTeamChildRunner, childCount int) {
	t.Helper()
	after, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || after.Members[memberID].Revision != memberRevision || after.Teams[teamID].Revision != teamRevision {
		t.Fatalf("retry changed team/member/request projection: before=%+v after=%+v", before, after)
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("retry appended facts: before=%d after=%d", eventCount, len(transcript.Events))
	}
	if got := runner.childCount(); got != childCount {
		t.Fatalf("retry started another follow-up: child count=%d want %d", got, childCount)
	}
}
