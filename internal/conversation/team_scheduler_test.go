package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

type capacitySignalSubmitter struct{ capacity chan struct{} }

func (s capacitySignalSubmitter) SubmitTaskCommitted(context.Context, agent.ParentRun, agent.DelegationTask, func(agent.TaskAdmission) error) (*agent.TaskHandle, error) {
	return nil, errors.New("unexpected submit")
}

func (s capacitySignalSubmitter) CapacityChanged() <-chan struct{} { return s.capacity }

func TestTeamCapacityQueueIsBoundedFIFOAndStopsAtFullHead(t *testing.T) {
	scheduler := &teamScheduler{wake: make(chan struct{}, 1)}
	var order []int
	full := true
	if err := scheduler.enqueueCapacityResume(func() error {
		order = append(order, 1)
		if full {
			return agent.ErrDelegationQueueFull
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.enqueueCapacityResume(func() error { order = append(order, 2); return nil }); err != nil {
		t.Fatal(err)
	}
	scheduler.drainCapacityQueue()
	if !reflect.DeepEqual(order, []int{1}) || len(scheduler.waiting) != 2 {
		t.Fatalf("full head was bypassed: order=%v waiting=%d", order, len(scheduler.waiting))
	}
	full = false
	scheduler.drainCapacityQueue()
	if !reflect.DeepEqual(order, []int{1, 1, 2}) || len(scheduler.waiting) != 0 {
		t.Fatalf("FIFO queue did not drain: order=%v waiting=%d", order, len(scheduler.waiting))
	}
}

func TestTeamCapacityQueueHasHardBound(t *testing.T) {
	scheduler := &teamScheduler{wake: make(chan struct{}, 1)}
	for i := 0; i < 32; i++ {
		if err := scheduler.enqueueCapacityResume(func() error { return nil }); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := scheduler.enqueueCapacityResume(func() error { return nil }); !errors.Is(err, agent.ErrDelegationQueueFull) {
		t.Fatalf("33rd waiting resume error=%v", err)
	}
}

func TestTeamSchedulerCloseStopsCapacityWatcher(t *testing.T) {
	scheduler := &teamScheduler{submitter: capacitySignalSubmitter{capacity: make(chan struct{})}, wake: make(chan struct{}, 1), done: make(chan struct{}), active: map[string]context.CancelFunc{}, roles: map[string]agentcatalog.Definition{}}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		scheduler.capacityLoop(context.Background())
	}()
	scheduler.close()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("scheduler close left its capacity watcher running")
	}
	scheduler.close()
}

type fixedTeamRoleCatalog struct{ definition agentcatalog.Definition }

func (c fixedTeamRoleCatalog) Snapshot() agentcatalog.Snapshot { return agentcatalog.Snapshot{} }
func (c fixedTeamRoleCatalog) Reload() agentcatalog.Snapshot   { return c.Snapshot() }
func (c fixedTeamRoleCatalog) Resolve(name string) (agentcatalog.Definition, bool) {
	if name != c.definition.Name {
		return agentcatalog.Definition{}, false
	}
	definition := c.definition
	definition.Tools = append([]string(nil), c.definition.Tools...)
	return definition, true
}

type capturingTeamChildRunner struct {
	inputs   chan agent.ChildRunInput
	mu       sync.Mutex
	children int
}

func (r *capturingTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.children++
	childNumber := r.children
	r.mu.Unlock()
	r.inputs <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: fmt.Sprintf("summary-%d", childNumber)}
}

func (r *capturingTeamChildRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.children
}

func TestTeamMemberContinuesAcrossRestartWithExplicitRoleChangeAcceptance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	request.Model = "model-v1"
	authority, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = authority
	roleV1 := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the first area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	catalogV1 := fixedTeamRoleCatalog{definition: roleV1}
	firstRunner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 2)}
	firstPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), firstRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = catalogV1
	service.deps.Delegator = firstPool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		firstPool.Close()
	})
	team, err := service.CreateTeam(t.Context(), request, "continued-work")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader", AgentName: roleV1.Name, Instruction: "Inspect area one.", OriginCallID: "call-spawn"})
	if err != nil {
		t.Fatal(err)
	}
	firstInput := receiveTeamChildInput(t, firstRunner.inputs)
	if firstInput.RoleInstruction != roleV1.Instruction || !strings.Contains(firstInput.Task.Instruction, roleV1.Instruction) {
		t.Fatalf("first turn did not receive its pinned role: %+v", firstInput)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	memberState := projection.Members[memberIDForName(t, projection, "reader")]
	if memberState.Summary != "summary-1" {
		t.Fatalf("first turn summary was not retained: %q", memberState.Summary)
	}
	if _, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: memberState.ID, Body: "Inspect the second area.", Token: "message-after-first-turn"}); err != nil {
		t.Fatal(err)
	}
	service.teamScheduler.close()
	firstPool.Close()
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[memberState.ID].Status != teams.MemberInterrupted {
		t.Fatalf("service restart member status=%s, want interrupted", projection.Members[memberState.ID].Status)
	}
	if firstRunner.count() != 1 {
		t.Fatalf("recovery reran provider work: child count=%d", firstRunner.count())
	}

	roleV2 := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the updated areas carefully.", Model: "model-v2", Tools: []string{"read_file"}, MaxTurns: 2}
	secondRunner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	secondPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), secondRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondService := &Service{deps: Deps{
		ProjectRoot: root, Agents: fixedTeamRoleCatalog{definition: roleV2}, Delegator: secondPool,
		ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v2",
	}, lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID}}
	secondService.teamScheduler = newTeamScheduler(secondService)
	t.Cleanup(func() {
		secondService.teamScheduler.close()
		secondPool.Close()
	})
	resumeRequest := request
	resumeRequest.Model = "model-v2"
	if _, err := secondService.ResumeTeamMember(t.Context(), resumeRequest, team.ID, memberState.ID, "call-resume-reject"); err == nil || !strings.Contains(err.Error(), "role definition fingerprint") || !strings.Contains(err.Error(), "model-v1 -> model-v2") {
		t.Fatalf("changed role was not reported before resume: %v", err)
	}
	if secondRunner.count() != 0 {
		t.Fatal("changed role ran without explicit acceptance")
	}
	resumeRequest.AcceptTeamRoleChange = true
	if _, err := secondService.ResumeTeamMember(t.Context(), resumeRequest, team.ID, memberState.ID, "call-resume-accept"); err != nil {
		t.Fatal(err)
	}
	secondInput := receiveTeamChildInput(t, secondRunner.inputs)
	if secondInput.RoleInstruction != roleV2.Instruction || !strings.Contains(secondInput.Task.Instruction, "summary-1") || !strings.Contains(secondInput.Task.Instruction, "Inspect the second area.") {
		t.Fatalf("resumed turn did not contain only the continued context and accepted role: %+v", secondInput)
	}
	if firstInput.TeamTurn == nil || secondInput.TeamTurn == nil || firstInput.TeamTurn.TeamID != secondInput.TeamTurn.TeamID || firstInput.TeamTurn.MemberID != secondInput.TeamTurn.MemberID || firstInput.TeamTurn.TurnID == secondInput.TeamTurn.TurnID {
		t.Fatalf("follow-up did not preserve the logical member with a fresh turn: first=%+v second=%+v", firstInput.TeamTurn, secondInput.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberState.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumed := projection.Members[memberState.ID]
	if resumed.Budget.AcceptedTurns != 2 || resumed.RoleHash == memberState.RoleHash || resumed.Model != "model-v2" {
		t.Fatalf("accepted role/turn state not persisted: %+v", resumed)
	}
	if secondRunner.count() != 1 {
		t.Fatalf("explicit resume ran %d child turns, want exactly one", secondRunner.count())
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), roleV1.Instruction) || strings.Contains(string(encoded), roleV2.Instruction) {
		t.Fatal("role instruction was persisted in the session log")
	}
}

func TestLeadMessageAutomaticallyResumesIdleTeamMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 2)}
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
	team, err := service.CreateTeam(t.Context(), request, "automatic-follow-up")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "call-spawn"})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	clientMsg := ClientMsg{
		Op: "team_send", SessionID: request.Work.SessionID, RunID: request.RunID,
		TeamID: team.ID, TeamRecipient: member.ID, TeamToken: "message-follow-up", Text: "Inspect area two.",
	}
	if err := validateClient(clientMsg); err != nil {
		t.Fatal(err)
	}
	response, err := service.handleTeamRequest(t.Context(), clientMsg)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != "team_send" || response.TeamMessage == nil {
		t.Fatalf("team_send handler response=%+v", response)
	}
	message := *response.TeamMessage
	if message.ID == "" || message.Seq == 0 || message.Body != clientMsg.Text || !reflect.DeepEqual(message.Recipients, []string{member.ID}) {
		t.Fatalf("team_send returned a different or unsequenced message: %+v", message)
	}
	second := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || second.TeamTurn == nil || first.TeamTurn.MemberID != second.TeamTurn.MemberID || first.TeamTurn.TurnID == second.TeamTurn.TurnID {
		t.Fatalf("automatic follow-up did not continue the same member: first=%+v second=%+v", first.TeamTurn, second.TeamTurn)
	}
	if second.TeamTurn.TeamID != team.ID || !strings.Contains(second.Task.Instruction, message.Body) {
		t.Fatalf("persisted lead message was not included as the follow-up handoff: %+v", second.Task)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages) != 1 || !reflect.DeepEqual(projection.Messages[message.ID], message) {
		t.Fatalf("team_send response and durable projection differ: response=%+v projection=%+v", message, projection.Messages)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if runner.count() != 2 {
		t.Fatalf("lead message started %d child turns, want one initial and one follow-up", runner.count())
	}
}

func memberIDForName(t *testing.T, projection sessionlog.TeamProjection, name string) string {
	t.Helper()
	for id, member := range projection.Members {
		if member.Name == name {
			return id
		}
	}
	t.Fatalf("member %q missing from projection", name)
	return ""
}

func receiveTeamChildInput(t *testing.T, inputs <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-inputs:
		return input
	case <-time.After(3 * time.Second):
		t.Fatal("fake child runner did not receive a team turn")
		return agent.ChildRunInput{}
	}
}

func waitForTeamMemberStatus(t *testing.T, root, sessionID, teamID, memberID string, status teams.MemberStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("member status=%s, want %s", projection.Members[memberID].Status, status)
}
