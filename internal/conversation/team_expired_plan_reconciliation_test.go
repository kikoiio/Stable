package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestExpiredPlanPreservesFailedTurnInterruption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "expired-plan-failed-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &failedGateTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ToolSchemas = []llm.ToolSchema{{Name: "team_plan_submit"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "failed-plan-expiry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "call-failed-expiry",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	team, err = service.GetTeam(t.Context(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(40 * time.Millisecond)
	pending, err := service.createTeamRequestUntil(root, team, child.ChildRunID, member.ID, member.ID, teams.RequestPlan, "inspect", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expiresAt) + 5*time.Millisecond)
	expired, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), "")
	if err == nil || expired.Status != teams.RequestExpired {
		t.Fatalf("expired plan response=%+v err=%v", expired, err)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberInterrupted)
	if _, err := service.ListTeamRequests(t.Context(), request, team.ID); err != nil {
		t.Fatalf("reconcile after failed turn: %v", err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID]; got.Status != teams.MemberInterrupted || got.PlanApproved {
		t.Fatalf("failed turn state=%+v, want interrupted and unapproved", got)
	}
	if runner.starts.Load() != 1 {
		t.Fatalf("expiry reconciliation started %d child turns, want one failed turn", runner.starts.Load())
	}
}

func TestTeamRequestExpiryTimerPersistsWithoutRequestTraffic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "plan-expiry-timer")
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
	service.deps.ToolSchemas = []llm.ToolSchema{{Name: "team_plan_submit"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "timer-expiry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "call-timer-expiry",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	team, err = service.GetTeam(t.Context(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(60 * time.Millisecond)
	pending, err := service.createTeamRequestUntil(root, team, child.ChildRunID, member.ID, member.ID, teams.RequestPlan, "inspect", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		projection, replayErr := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if replayErr == nil && projection.Requests[pending.ID].Status == teams.RequestExpired {
			if got := projection.Members[member.ID]; got.Status != teams.MemberRunning {
				t.Fatalf("expiry timer changed active member to %s", got.Status)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[pending.ID]; got.Status != teams.RequestExpired {
		t.Fatalf("request remained %s without list/respond traffic, want expired", got.Status)
	}
	if runner.childCount() != 1 {
		t.Fatalf("expiry timer started %d child turns, want only the active turn", runner.childCount())
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
}

type failedGateTeamChildRunner struct {
	inputs  chan agent.ChildRunInput
	release chan struct{}
	starts  atomic.Int32
}

func (r *failedGateTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.starts.Add(1)
	r.inputs <- input
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "fixture failure"}
}
