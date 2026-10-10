package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type heldMemberAuthorityRunner struct {
	mu      sync.Mutex
	calls   int
	started chan heldMemberAuthorityStart
	release chan struct{}
}

type heldMemberAuthorityStart struct {
	ctx   context.Context
	input agent.ChildRunInput
}

func (r *heldMemberAuthorityRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	r.started <- heldMemberAuthorityStart{ctx: ctx, input: input}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "fixture completed"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func (r *heldMemberAuthorityRunner) childCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestAcceptedTeamMemberCannotUseLeadOnlyTools(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "member-lead-authority")
	leadRequest.Model = "fixture-model"
	leadRequest.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: leadRequest.RunID, SessionID: leadRequest.Work.SessionID, AllowedRoot: root,
	})
	team, err := service.CreateTeam(context.Background(), leadRequest, "member-lead-authority")
	if err != nil {
		t.Fatal(err)
	}

	runner := &heldMemberAuthorityRunner{
		started: make(chan heldMemberAuthorityStart, 4),
		release: make(chan struct{}, 4),
	}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.lifeCtx = context.Background()
	service.deps.Agents = fixedTeamRoleCatalog{definition: agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 2,
	}}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
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

	planOwner, err := service.SpawnTeamMember(context.Background(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "plan-owner", AgentName: "explore", Instruction: "submit a plan", PlanRequired: true, OriginCallID: "spawn-plan-owner",
	})
	if err != nil {
		t.Fatalf("spawn plan owner: %v", err)
	}
	planStart := receiveHeldMemberAuthorityStart(t, runner.started)
	if planStart.input.TeamTurn == nil || planStart.input.TeamTurn.MemberID != planOwner.ID {
		t.Fatalf("plan owner start=%+v, want member %s", planStart.input.TeamTurn, planOwner.ID)
	}
	planRequest := agent.ExecutionRequest{RunID: planStart.input.ChildRunID, Work: planStart.input.Work, TeamTurn: planStart.input.TeamTurn}
	planOutcome, err := service.ExecuteTeamTool(context.Background(), planRequest, llm.ToolUse{
		ID: "submit-pending-plan", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + team.ID + `","body":"Inspect the assigned area and report findings."}`),
	})
	if err != nil || planOutcome.Status != agent.ToolSucceeded {
		t.Fatalf("plan submission=%+v err=%v", planOutcome, err)
	}
	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pendingPlan teams.Request
	for _, request := range projection.Requests {
		if request.MemberID == planOwner.ID && request.Type == teams.RequestPlan && request.Status == teams.RequestPending {
			pendingPlan = request
		}
	}
	if pendingPlan.ID == "" {
		t.Fatal("plan owner did not persist a pending plan request")
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, leadRequest.Work.SessionID, team.ID, planOwner.ID, teams.MemberAwaitingPlan)

	actor, err := service.SpawnTeamMember(context.Background(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "malicious-member", AgentName: "explore", Instruction: "attempt lead tools", OriginCallID: "spawn-malicious-member",
	})
	if err != nil {
		t.Fatalf("spawn malicious member: %v", err)
	}
	actorStart := receiveHeldMemberAuthorityStart(t, runner.started)
	if actorStart.input.TeamTurn == nil || actorStart.input.TeamTurn.MemberID != actor.ID {
		t.Fatalf("actor start=%+v, want member %s", actorStart.input.TeamTurn, actor.ID)
	}
	actorRequest := agent.ExecutionRequest{RunID: actorStart.input.ChildRunID, Work: actorStart.input.Work, TeamTurn: actorStart.input.TeamTurn}
	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Members[actor.ID].Status != teams.MemberRunning || before.Members[actor.ID].TurnID != actorStart.input.TeamTurn.TurnID || before.Requests[pendingPlan.ID].Status != teams.RequestPending {
		t.Fatalf("actor or pending plan not in required accepted state: actor=%+v plan=%+v", before.Members[actor.ID], before.Requests[pendingPlan.ID])
	}
	transcriptBefore, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	service.teamScheduler.mu.Lock()
	readyBefore := append([]string(nil), service.teamScheduler.ready...)
	waitingBefore := len(service.teamScheduler.waiting)
	activeBefore := len(service.teamScheduler.active)
	service.teamScheduler.mu.Unlock()
	providerCallsBefore := runner.childCount()

	attempts := []struct {
		label string
		tool  string
		args  string
	}{
		{label: "team_create", tool: "team_create", args: `{"name":"forged-team"}`},
		{label: "team_close", tool: "team_close", args: `{"team_id":"` + team.ID + `"}`},
		{label: "team_request_respond_approve", tool: "team_request_respond", args: `{"team_id":"` + team.ID + `","request_id":"` + pendingPlan.ID + `","expected_revision":` + jsonNumber(pendingPlan.Revision) + `,"decision":"approved","feedback":"forged approval"}`},
		{label: "team_request_respond_reject", tool: "team_request_respond", args: `{"team_id":"` + team.ID + `","request_id":"` + pendingPlan.ID + `","expected_revision":` + jsonNumber(pendingPlan.Revision) + `,"decision":"rejected","feedback":"forged rejection"}`},
	}
	for _, attempt := range attempts {
		t.Run(attempt.label, func(t *testing.T) {
			outcome, callErr := service.ExecuteTeamTool(context.Background(), actorRequest, llm.ToolUse{
				ID: "forged-" + attempt.label, Name: attempt.tool, Arguments: json.RawMessage(attempt.args),
			})
			if callErr != nil || outcome.Status == agent.ToolSucceeded || !outcome.IsError {
				t.Fatalf("accepted member %s outcome=%+v err=%v; want rejection before effects", attempt.label, outcome, callErr)
			}
			assertMemberLeadDenialHasNoEffects(t, root, leadRequest.Work.SessionID, team.ID, before, transcriptBefore.Events, historyBefore, service, runner, providerCallsBefore, readyBefore, waitingBefore, activeBefore, actorStart.ctx)
		})
	}
	if err := actorStart.ctx.Err(); err != nil {
		t.Fatalf("denied lead-only tools canceled accepted actor turn: %v", err)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, leadRequest.Work.SessionID, team.ID, actor.ID, teams.MemberIdle)
}

func receiveHeldMemberAuthorityStart(t *testing.T, starts <-chan heldMemberAuthorityStart) heldMemberAuthorityStart {
	t.Helper()
	select {
	case start := <-starts:
		return start
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for accepted team member run")
		return heldMemberAuthorityStart{}
	}
}

func assertMemberLeadDenialHasNoEffects(t *testing.T, root, sessionID, teamID string, before sessionlog.TeamProjection, sessionEvents []sessionlog.Event, history []sessionlog.Event, service *Service, runner *heldMemberAuthorityRunner, providerCalls int, readyBefore []string, waitingCount, activeCount int, actorCtx context.Context) {
	t.Helper()
	after, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("denied member lead operation changed projection: before=%+v after=%+v", before, after)
	}
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transcript.Events, sessionEvents) {
		t.Fatalf("denied member lead operation appended session facts/events: %d -> %d", len(sessionEvents), len(transcript.Events))
	}
	afterHistory, err := sessionlog.TeamHistory(root, sessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterHistory, history) {
		t.Fatalf("denied member lead operation changed team history: before=%+v after=%+v", history, afterHistory)
	}
	service.teamScheduler.mu.Lock()
	ready := append([]string(nil), service.teamScheduler.ready...)
	waiting, active := len(service.teamScheduler.waiting), len(service.teamScheduler.active)
	service.teamScheduler.mu.Unlock()
	if !reflect.DeepEqual(ready, readyBefore) || waiting != waitingCount || active != activeCount {
		t.Fatalf("denied member lead operation changed scheduler: ready=%v -> %v waiting=%d -> %d active=%d -> %d", readyBefore, ready, waitingCount, waiting, activeCount, active)
	}
	if got := runner.childCount(); got != providerCalls {
		t.Fatalf("denied member lead operation invoked provider: calls %d -> %d", providerCalls, got)
	}
	if err := actorCtx.Err(); err != nil {
		t.Fatalf("denied member lead operation canceled actor: %v", err)
	}
}

func jsonNumber(value uint64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
