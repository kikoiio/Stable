package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type gatedTeamChildRunner struct {
	inputs  chan agent.ChildRunInput
	release chan struct{}
	mu      sync.Mutex
	started int
}

type interruptedHandoffRunner struct {
	mu     sync.Mutex
	inputs chan agent.ChildRunInput
	calls  int
}

func (r *interruptedHandoffRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	r.inputs <- input
	if call == 2 {
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: "fixture interruption after message handoff"}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed"}
}

func (r *interruptedHandoffRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *gatedTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.started++
	r.mu.Unlock()
	r.inputs <- input
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed"}
}

func TestTeamMessageSentDuringRunningTurnIsHandedOffOnlyToNextTurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
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
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "running-turn-handoff")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "call-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	message, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "Inspect area two.", Token: "during-running-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Task.Instruction, message.Body) {
		t.Fatalf("message sent after turn start was inserted into the running turn: %+v", first.Task)
	}
	if got := runner.childCount(); got != 1 {
		t.Fatalf("message sent to running member started another turn: children=%d", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "call-resume"); err != nil {
		t.Fatal(err)
	}
	second := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || second.TeamTurn == nil || first.TeamTurn.MemberID != second.TeamTurn.MemberID || first.TeamTurn.TurnID == second.TeamTurn.TurnID {
		t.Fatalf("follow-up did not continue the same logical member: first=%+v second=%+v", first.TeamTurn, second.TeamTurn)
	}
	if !strings.Contains(second.Task.Instruction, message.Body) {
		t.Fatalf("next turn did not receive the pending message: %+v", second.Task)
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handoffCount int
	for _, handoff := range projection.Handoffs {
		if handoff.MessageID == message.ID {
			handoffCount++
			if handoff.DestinationTurnID != second.TeamTurn.TurnID || handoff.RetryOfTurnID != "" {
				t.Fatalf("message handoff points at wrong destination: %+v", handoff)
			}
		}
	}
	if handoffCount != 1 {
		t.Fatalf("pending message has %d durable handoffs, want exactly one", handoffCount)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
}

func TestInterruptedMessageHandoffIsExplicitlyRetriedAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &interruptedHandoffRunner{inputs: make(chan agent.ChildRunInput, 3)}
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

	team, err := service.CreateTeam(t.Context(), request, "message-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "spawn"})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial member turn=%+v", first)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	message, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: member.ID, Body: "resume with the parser edge case", Token: "message-before-interrupted-handoff"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-interrupted"); err != nil {
		t.Fatal(err)
	}
	interrupted := receiveTeamChildInput(t, runner.inputs)
	if interrupted.TeamTurn == nil || !strings.Contains(interrupted.Task.Instruction, message.Body) {
		t.Fatalf("message was not included in the interrupted destination turn: %+v", interrupted)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberInterrupted)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	if runner.count() != 2 {
		t.Fatalf("startup recovery replayed provider work: calls=%d", runner.count())
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var priorHandoff sessionlog.HandoffFact
	priorCount := 0
	for _, handoff := range projection.Handoffs {
		if handoff.MessageID == message.ID {
			priorHandoff = handoff
			priorCount++
		}
	}
	if priorCount != 1 || priorHandoff.DestinationTurnID != interrupted.TeamTurn.TurnID {
		t.Fatalf("interrupted destination handoff facts=%+v count=%d", priorHandoff, priorCount)
	}

	retryRunner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	retryPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), retryRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{
		deps: Deps{ProjectRoot: root, Agents: fixedTeamRoleCatalog{definition: role}, Delegator: retryPool,
			ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
			ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v1"},
		lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID},
		activeRequests: map[string]agent.ExecutionRequest{request.RunID: request}}
	restarted.teamScheduler = newTeamScheduler(restarted)
	t.Cleanup(func() {
		restarted.teamScheduler.close()
		retryPool.Close()
	})
	if _, err := restarted.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-after-restart"); err != nil {
		t.Fatal(err)
	}
	retry := receiveTeamChildInput(t, retryRunner.inputs)
	if retry.TeamTurn == nil || retry.TeamTurn.TurnID == interrupted.TeamTurn.TurnID || strings.Count(retry.Task.Instruction, message.Body) != 1 {
		t.Fatalf("explicit retry did not carry one copy into a new turn: %+v", retry)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handoffs []sessionlog.HandoffFact
	for _, handoff := range projection.Handoffs {
		if handoff.MessageID == message.ID {
			handoffs = append(handoffs, handoff)
		}
	}
	if len(handoffs) != 2 || handoffs[1].DestinationTurnID != retry.TeamTurn.TurnID || handoffs[1].RetryOfTurnID != interrupted.TeamTurn.TurnID {
		t.Fatalf("retry handoff chain=%+v", handoffs)
	}
}

func (r *gatedTeamChildRunner) childCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

var _ agent.ChildRunner = (*gatedTeamChildRunner)(nil)
