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

func (r *gatedTeamChildRunner) childCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

var _ agent.ChildRunner = (*gatedTeamChildRunner)(nil)
