package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

type teamChildStart struct {
	input agent.ChildRunInput
	ctx   context.Context
}

type cancelAwareTeamChildRunner struct {
	started  chan teamChildStart
	canceled chan string
}

func (r *cancelAwareTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- teamChildStart{input: input, ctx: ctx}
	<-ctx.Done()
	r.canceled <- input.TeamTurn.TurnID
	return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
}

type recordingParentRunner struct {
	mu       sync.Mutex
	canceled []string
}

func (*recordingParentRunner) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, nil
}

func (r *recordingParentRunner) Cancel(runID string) error {
	r.mu.Lock()
	r.canceled = append(r.canceled, runID)
	r.mu.Unlock()
	return nil
}

func (r *recordingParentRunner) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.canceled...)
}

func TestCancelParentRunCancelsOnlyItsTeamChildren(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, parentA := teamServiceFixture(t, root, "parent-a")
	parentBID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, parentA.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: parentBID, WorkKind: string(agent.WorkSession), Intent: "second parent fixture",
	}); err != nil {
		t.Fatal(err)
	}
	parentB := parentA
	parentB.RunID = parentBID
	parentB.PermissionBounds, err = json.Marshal(permission.Authority{
		RunID: parentB.RunID, SessionID: parentB.Work.SessionID, AllowedRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentA.PermissionBounds, err = json.Marshal(permission.Authority{
		RunID: parentA.RunID, SessionID: parentA.Work.SessionID, AllowedRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.activeRuns[parentB.RunID] = parentB.Work.SessionID
	service.activeRequests = map[string]agent.ExecutionRequest{parentA.RunID: parentA, parentB.RunID: parentB}
	service.mu.Unlock()

	childRunner := &cancelAwareTeamChildRunner{started: make(chan teamChildStart, 2), canceled: make(chan string, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentRunner := &recordingParentRunner{}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	service.deps.Runner = parentRunner
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

	team, err := service.CreateTeam(t.Context(), parentA, "parent-cancel-scope")
	if err != nil {
		t.Fatal(err)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), parentA, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-a", AgentName: role.Name, Instruction: "Inspect area A.", OriginCallID: "spawn-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberB, err := service.SpawnTeamMember(t.Context(), parentB, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-b", AgentName: role.Name, Instruction: "Inspect area B.", OriginCallID: "spawn-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	startA := receiveTeamChildStart(t, childRunner.started)
	startB := receiveTeamChildStart(t, childRunner.started)
	if startA.input.TeamTurn == nil || startB.input.TeamTurn == nil {
		t.Fatalf("child start lacks team identity: %+v / %+v", startA.input.TeamTurn, startB.input.TeamTurn)
	}
	starts := map[string]teamChildStart{startA.input.TeamTurn.MemberID: startA, startB.input.TeamTurn.MemberID: startB}
	startA, okA := starts[memberA.ID]
	startB, okB := starts[memberB.ID]
	if !okA || !okB {
		t.Fatalf("children started for unexpected members: %+v", starts)
	}
	waitForTeamMemberStatus(t, root, parentA.Work.SessionID, team.ID, memberA.ID, teams.MemberRunning)
	waitForTeamMemberStatus(t, root, parentA.Work.SessionID, team.ID, memberB.ID, teams.MemberRunning)

	if err := service.cancelRun(ClientMsg{RunID: parentA.RunID, SessionID: parentA.Work.SessionID}, make(chan ServerMsg, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-startA.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("parent A cancellation did not cancel its child context")
	}
	select {
	case <-startB.ctx.Done():
		t.Fatal("parent A cancellation canceled parent B's child context")
	default:
	}
	select {
	case turnID := <-childRunner.canceled:
		if turnID != startA.input.TeamTurn.TurnID {
			t.Fatalf("canceled turn=%s, want parent A turn %s", turnID, startA.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent A child did not report cancellation")
	}
	if got := parentRunner.calls(); len(got) != 1 || got[0] != parentA.RunID {
		t.Fatalf("parent Runner.Cancel calls after A cancel = %v, want [%s]", got, parentA.RunID)
	}
	waitForTeamMemberStatus(t, root, parentA.Work.SessionID, team.ID, memberA.ID, teams.MemberInterrupted)
	projection, err := sessionlog.ReplayTeams(root, parentA.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Turns[startA.input.TeamTurn.TurnID].Status != string(agent.DelegationCanceled) {
		t.Fatalf("parent A turn status=%s, want canceled", projection.Turns[startA.input.TeamTurn.TurnID].Status)
	}
	if projection.Members[memberB.ID].Status != teams.MemberRunning || projection.Turns[startB.input.TeamTurn.TurnID].Status != "queued" {
		t.Fatalf("parent A cancel changed parent B state: member=%s turn=%s", projection.Members[memberB.ID].Status, projection.Turns[startB.input.TeamTurn.TurnID].Status)
	}
	select {
	case turnID := <-childRunner.canceled:
		t.Fatalf("parent B child canceled before its own parent: turn=%s", turnID)
	default:
	}

	if err := service.cancelRun(ClientMsg{RunID: parentB.RunID, SessionID: parentB.Work.SessionID}, make(chan ServerMsg, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-startB.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("parent B cancellation did not cancel its child context")
	}
	select {
	case turnID := <-childRunner.canceled:
		if turnID != startB.input.TeamTurn.TurnID {
			t.Fatalf("canceled turn=%s, want parent B turn %s", turnID, startB.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent B child did not report cancellation")
	}
	waitForTeamMemberStatus(t, root, parentA.Work.SessionID, team.ID, memberB.ID, teams.MemberInterrupted)
	if got := parentRunner.calls(); len(got) != 2 || got[0] != parentA.RunID || got[1] != parentB.RunID {
		t.Fatalf("parent Runner.Cancel calls = %v, want [%s %s]", got, parentA.RunID, parentB.RunID)
	}
}

func TestServiceCloseCancelsTeamChildAndDrainsWatcher(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "service close team fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: parentRunID, WorkKind: string(agent.WorkSession), Intent: "service close fixture",
	}); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "m09-close-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	childRunner := &cancelAwareTeamChildRunner{started: make(chan teamChildStart, 1), canceled: make(chan string, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	service, err := Serve(context.Background(), Deps{
		ProjectRoot: root, SocketPath: filepath.Join(socketDir, "s.sock"), PollEvery: time.Hour,
		Agents: fixedTeamRoleCatalog{definition: role}, Delegator: pool,
		ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v1",
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	parent := agent.ExecutionRequest{RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}}
	parent.PermissionBounds, err = json.Marshal(permission.Authority{RunID: parentRunID, SessionID: session.ID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.activeRuns[parentRunID] = session.ID
	service.activeRequests[parentRunID] = parent
	service.mu.Unlock()

	t.Cleanup(func() {
		_ = service.Close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), parent, "service-close")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), parent, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned files.", OriginCallID: "spawn-close",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := receiveTeamChildStart(t, childRunner.started)
	if started.input.TeamTurn == nil || started.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected active team child: %+v", started.input.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberRunning)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case turnID := <-childRunner.canceled:
		if turnID != started.input.TeamTurn.TurnID {
			t.Fatalf("canceled turn=%s, want %s", turnID, started.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service close did not cancel the active team child")
	}
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberInterrupted)
	deadline := time.Now().Add(3 * time.Second)
	for {
		service.teamScheduler.mu.Lock()
		active := len(service.teamScheduler.active) + len(service.teamScheduler.activeOrigin) + len(service.teamScheduler.activeMember) + len(service.teamScheduler.activeTeam)
		service.teamScheduler.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("team watcher did not drain after child exit: active registrations=%d", active)
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[started.input.TeamTurn.TurnID].Status; got != string(agent.DelegationCanceled) {
		t.Fatalf("closed-service turn status=%s, want canceled", got)
	}
}

func receiveTeamChildStart(t *testing.T, starts <-chan teamChildStart) teamChildStart {
	t.Helper()
	select {
	case start := <-starts:
		return start
	case <-time.After(3 * time.Second):
		t.Fatal("team child did not reach the start barrier")
		return teamChildStart{}
	}
}
