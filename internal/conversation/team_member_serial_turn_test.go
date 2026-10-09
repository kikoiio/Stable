package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/teams"
)

type serialTeamTurnRunner struct {
	started chan agent.ChildRunInput
	release chan struct{}
	mu      sync.Mutex
	active  int
	max     int
	count   int
}

func (r *serialTeamTurnRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.active++
	if r.active > r.max {
		r.max = r.active
	}
	r.count++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "turn-summary"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func TestTeamMemberQueuesMessagesWithoutParallelTurns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	request.Model = "model-v1"
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 4}
	runner := &serialTeamTurnRunner{started: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 4)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", request.Model
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	team, err := service.CreateTeam(t.Context(), request, "serial-member-turns")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the first area.", OriginCallID: "spawn-reader"})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.started)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("first turn identity: %+v", first.TeamTurn)
	}
	for i, body := range []string{"Inspect the second area.", "Compare the third area."} {
		if _, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: member.ID, Body: body, Token: "serial-message-" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)
	select {
	case duplicate := <-runner.started:
		t.Fatalf("member started an overlapping turn: %+v", duplicate.TeamTurn)
	case <-time.After(25 * time.Millisecond):
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	runner.mu.Lock()
	countBeforeResume := runner.count
	runner.mu.Unlock()
	if countBeforeResume != 1 {
		t.Fatalf("busy member auto-started a follow-up turn before explicit resume: count=%d", countBeforeResume)
	}
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "explicit-resume-after-busy-messages"); err != nil {
		t.Fatal(err)
	}
	second := receiveTeamChildInput(t, runner.started)
	if second.TeamTurn == nil || second.TeamTurn.MemberID != member.ID || second.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("queued message did not get a fresh turn for the same member: first=%+v second=%+v", first.TeamTurn, second.TeamTurn)
	}
	if !strings.Contains(second.Task.Instruction, "Inspect the second area.") || !strings.Contains(second.Task.Instruction, "Compare the third area.") {
		t.Fatalf("queued messages were not handed to the next turn: %q", second.Task.Instruction)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	runner.mu.Lock()
	max, count := runner.max, runner.count
	runner.mu.Unlock()
	if max != 1 || count != 2 {
		t.Fatalf("member turn concurrency/count = %d/%d, want one active at a time and two turns", max, count)
	}
}
