package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestSubmitTeamPlanEnforcesExactBodyLimitWithoutPartialFacts(t *testing.T) {
	fixture := newTeamRequestSizeLimitFixture(t, true)
	body := strings.Repeat("p", teams.MaxPlanBytes)
	created, err := fixture.service.SubmitTeamPlan(t.Context(), fixture.childRequest, fixture.team.ID, body)
	if err != nil || created.Status != teams.RequestPending || len(created.Body) != teams.MaxPlanBytes {
		t.Fatalf("plan body at exact limit was not accepted: request=%+v err=%v", created, err)
	}
	assertOversizeRequestIsNoOp(t, fixture, func() error {
		_, err := fixture.service.SubmitTeamPlan(t.Context(), fixture.childRequest, fixture.team.ID, strings.Repeat("x", teams.MaxPlanBytes+1))
		return err
	})
	projection, err := sessionlog.ReplayTeams(fixture.root, fixture.request.Work.SessionID, fixture.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Requests) != 1 || projection.Requests[created.ID].Body != body {
		t.Fatalf("oversized plan changed the accepted request set: %+v", projection.Requests)
	}
	fixture.runner.release <- struct{}{}
	waitForTeamMemberStatus(t, fixture.root, fixture.request.Work.SessionID, fixture.team.ID, fixture.member.ID, teams.MemberAwaitingPlan)
}

func TestRespondTeamRequestEnforcesExactFeedbackLimitWithoutPartialFacts(t *testing.T) {
	fixture := newTeamRequestSizeLimitFixture(t, false)
	shutdown, err := fixture.service.RequestTeamShutdown(t.Context(), fixture.request, fixture.team.ID, fixture.member.ID)
	if err != nil || shutdown.Status != teams.RequestPending {
		t.Fatalf("busy shutdown request = %+v, err=%v", shutdown, err)
	}
	feedback := strings.Repeat("f", teams.MaxFeedbackBytes)
	deferred, err := fixture.service.RespondTeamRequest(t.Context(), fixture.childRequest, fixture.team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), feedback)
	if err != nil || deferred.Status != teams.RequestDeferred || len(deferred.Feedback) != teams.MaxFeedbackBytes {
		t.Fatalf("feedback at exact limit was not accepted: request=%+v err=%v", deferred, err)
	}
	assertOversizeRequestIsNoOp(t, fixture, func() error {
		_, err := fixture.service.RespondTeamRequest(t.Context(), fixture.childRequest, fixture.team.ID, shutdown.ID, deferred.Revision, string(teams.RequestDeferred), strings.Repeat("x", teams.MaxFeedbackBytes+1))
		return err
	})
	projection, err := sessionlog.ReplayTeams(fixture.root, fixture.request.Work.SessionID, fixture.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[shutdown.ID]; got.Status != teams.RequestDeferred || got.Feedback != feedback || got.Revision != deferred.Revision {
		t.Fatalf("oversized feedback changed the deferred request: %+v", got)
	}
	if _, err := fixture.service.RespondTeamRequest(t.Context(), fixture.childRequest, fixture.team.ID, shutdown.ID, deferred.Revision, string(teams.RequestRejected), "Continue the assigned work."); err != nil {
		t.Fatal(err)
	}
	fixture.runner.release <- struct{}{}
	waitForTeamMemberStatus(t, fixture.root, fixture.request.Work.SessionID, fixture.team.ID, fixture.member.ID, teams.MemberIdle)
}

type teamRequestSizeLimitFixture struct {
	root         string
	service      *Service
	request      agent.ExecutionRequest
	childRequest agent.ExecutionRequest
	team         teams.Team
	member       teams.Member
	runner       *gatedTeamChildRunner
}

func newTeamRequestSizeLimitFixture(t *testing.T, planRequired bool) teamRequestSizeLimitFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "request-size-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
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

	team, err := service.CreateTeam(t.Context(), request, "request-size-boundary")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.",
		PlanRequired: planRequired, OriginCallID: "request-size-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	if child.TeamTurn == nil || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected child turn: %+v", child.TeamTurn)
	}
	return teamRequestSizeLimitFixture{
		root: root, service: service, request: request,
		childRequest: agent.ExecutionRequest{RunID: child.ChildRunID, Work: request.Work, TeamTurn: child.TeamTurn},
		team:         team, member: member, runner: runner,
	}
}

func assertOversizeRequestIsNoOp(t *testing.T, fixture teamRequestSizeLimitFixture, attempt func() error) {
	t.Helper()
	beforeProjection, err := sessionlog.ReplayTeams(fixture.root, fixture.request.Work.SessionID, fixture.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(fixture.root, fixture.request.Work.SessionID, fixture.team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(fixture.root, fixture.request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt(); err == nil {
		t.Fatal("oversized team request was accepted")
	}
	afterProjection, err := sessionlog.ReplayTeams(fixture.root, fixture.request.Work.SessionID, fixture.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) {
		t.Fatalf("oversized request changed team projection: before=%+v after=%+v", beforeProjection, afterProjection)
	}
	afterHistory, err := sessionlog.TeamHistory(fixture.root, fixture.request.Work.SessionID, fixture.team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessionlog.Replay(fixture.root, fixture.request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterHistory) != len(beforeHistory) || len(afterSession.Events) != len(beforeSession.Events) {
		t.Fatalf("oversized request appended facts: history %d -> %d; session events %d -> %d", len(beforeHistory), len(afterHistory), len(beforeSession.Events), len(afterSession.Events))
	}
}
