package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type sharedPoolGateRunner struct {
	started chan agent.ChildRunInput
	release chan struct{}
}

func (r *sharedPoolGateRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	if input.TeamTurn == nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
		}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "gate-runner-complete"}
}

func TestTeamCapacityUsesSharedPoolAndResumesWaitingMessagesFairly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	request, err := newCapacityTestRun(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		deps: Deps{ProjectRoot: root, Agents: fixedTeamRoleCatalog{definition: agentcatalog.Definition{
			Name: "explore", Instruction: "inspect assigned material", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3,
		}}, ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
			ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v1"},
		lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID},
		activeRequests: map[string]agent.ExecutionRequest{request.RunID: request},
	}
	runner := &sharedPoolGateRunner{started: make(chan agent.ChildRunInput, 16), release: make(chan struct{}, 4)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Delegator = pool
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	team, err := service.CreateTeam(t.Context(), request, "capacity-gate")
	if err != nil {
		t.Fatal(err)
	}
	parent := agent.ParentRun{
		RunID: request.RunID, Work: request.Work, ProjectRoot: root,
		PermissionBounds: append(json.RawMessage(nil), request.PermissionBounds...),
		Provider:         forkSkillFixtureProvider{}, Model: "model-v1",
	}

	first, err := pool.SubmitTask(t.Context(), parent, agent.DelegationTask{ID: "pool-running", Name: "pool-running", Instruction: "occupy the only worker"})
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveSharedPoolInput(t, runner.started); got.Task.ID != "pool-running" {
		t.Fatalf("first pool task=%+v", got.Task)
	}
	second, err := pool.SubmitTask(t.Context(), parent, agent.DelegationTask{ID: "pool-queued", Name: "pool-queued", Instruction: "occupy the only queue slot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "rejected", AgentName: "explore", Instruction: "must not be accepted", OriginCallID: "call-rejected"}); !errors.Is(err, agent.ErrDelegationQueueFull) {
		t.Fatalf("first member spawn under full shared pool=%v, want queue-full", err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil || len(projection.Members) != 0 {
		t.Fatalf("rejected first spawn left team member facts: members=%+v err=%v", projection.Members, err)
	}

	runner.release <- struct{}{}
	if got := receiveSharedPoolInput(t, runner.started); got.Task.ID != "pool-queued" {
		t.Fatalf("queued pool task bypassed FIFO: %+v", got.Task)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader-a", AgentName: "explore", Instruction: "inspect area a", OriginCallID: "call-a"})
	if err != nil || memberA.Status != teams.MemberQueued {
		t.Fatalf("member A spawn after capacity released: member=%+v err=%v", memberA, err)
	}
	runner.release <- struct{}{}
	initialA := receiveSharedPoolInput(t, runner.started)
	if initialA.TeamTurn == nil || initialA.TeamTurn.MemberID != memberA.ID {
		t.Fatalf("member A did not run through the shared pool: %+v", initialA)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	memberB, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader-b", AgentName: "explore", Instruction: "inspect area b", OriginCallID: "call-b"})
	if err != nil {
		t.Fatal(err)
	}
	initialB := receiveSharedPoolInput(t, runner.started)
	if initialB.TeamTurn == nil || initialB.TeamTurn.MemberID != memberB.ID {
		t.Fatalf("member B did not run through the shared pool: %+v", initialB)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)

	occupier, err := pool.SubmitTask(t.Context(), parent, agent.DelegationTask{ID: "handoff-running", Name: "handoff-running", Instruction: "hold worker"})
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveSharedPoolInput(t, runner.started); got.Task.ID != "handoff-running" {
		t.Fatalf("handoff occupier=%+v", got.Task)
	}
	queued, err := pool.SubmitTask(t.Context(), parent, agent.DelegationTask{ID: "handoff-queued", Name: "handoff-queued", Instruction: "hold queue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: memberA.ID, Body: "continue area a", Token: "capacity-message-a"}); err != nil {
		t.Fatal(err)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberWaitingCapacity)
	if _, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: memberB.ID, Body: "continue area b", Token: "capacity-message-b"}); err != nil {
		t.Fatal(err)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberWaitingCapacity)

	runner.release <- struct{}{}
	if got := receiveSharedPoolInput(t, runner.started); got.Task.ID != "handoff-queued" {
		t.Fatalf("second pool FIFO task bypassed: %+v", got.Task)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberQueued)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberWaitingCapacity)
	runner.release <- struct{}{}
	followA := receiveSharedPoolInput(t, runner.started)
	followB := receiveSharedPoolInput(t, runner.started)
	if followA.TeamTurn == nil || followA.TeamTurn.MemberID != memberA.ID || !strings.Contains(followA.Task.Instruction, "continue area a") {
		t.Fatalf("FIFO capacity retry did not admit member A with its message first: %+v", followA)
	}
	if followB.TeamTurn == nil || followB.TeamTurn.MemberID != memberB.ID || !strings.Contains(followB.Task.Instruction, "continue area b") {
		t.Fatalf("member B message was lost or overtook member A: %+v", followB)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[memberA.ID].Budget.AcceptedTurns != 2 || projection.Members[memberB.ID].Budget.AcceptedTurns != 2 {
		t.Fatalf("capacity retries did not preserve one accepted turn per message: A=%+v B=%+v", projection.Members[memberA.ID], projection.Members[memberB.ID])
	}
	history, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handoffs := 0
	for _, event := range history {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff {
			handoffs++
		}
	}
	if handoffs != 2 {
		t.Fatalf("capacity replay persisted %d message handoffs, want exactly two", handoffs)
	}
	for _, handle := range []*agent.TaskHandle{first, second, occupier, queued} {
		if handle != nil && handle.Cancel != nil {
			handle.Cancel()
		}
	}
}

func newCapacityTestRun(root, runID string) (agent.ExecutionRequest, error) {
	session, err := sessionlog.Create(root, "shared pool capacity")
	if err != nil {
		return agent.ExecutionRequest{}, err
	}
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: string(work.Kind), Intent: "capacity integration"}); err != nil {
		return agent.ExecutionRequest{}, err
	}
	authority, err := json.Marshal(permission.Authority{RunID: runID, SessionID: session.ID, AllowedRoot: root})
	if err != nil {
		return agent.ExecutionRequest{}, err
	}
	return agent.ExecutionRequest{RunID: runID, Work: work, PermissionBounds: authority, Model: "model-v1"}, nil
}

func receiveSharedPoolInput(t *testing.T, inputs <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-inputs:
		return input
	case <-time.After(3 * time.Second):
		t.Fatal("shared pool did not start queued child work")
		return agent.ChildRunInput{}
	}
}
