package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

type terminalIndependentTeamChildRunner struct {
	started chan teamChildStart
	release chan struct{}
}

func (r *terminalIndependentTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- teamChildStart{input: input, ctx: ctx}
	<-r.release
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "completed after parent"}
}

func TestTeamChildContinuesAfterParentRunCompletes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-completed")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
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
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "parent-completion")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.", OriginCallID: "spawn-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)

	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-terminal", RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("parent completion canceled accepted team child: %v", child.ctx.Err())
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("parent completion changed child member status to %s, want running", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn status after parent completion = %s, want succeeded", got)
	}
}

func TestTeamChildContinuesAfterParentRunFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-failed")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
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
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "parent-failure")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.", OriginCallID: "spawn-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)

	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "parent-terminal", RunID: request.RunID, SessionID: request.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("parent failure canceled accepted team child: %v", child.ctx.Err())
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("parent failure changed child member status to %s, want running", got)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn status after parent failure = %s, want succeeded", got)
	}
}

func TestTeamChildContinuesAfterParentClientDisconnects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "disconnect recovery")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	parentEvents := make(chan agent.ExecutionEvent, 1)
	parentDone := make(chan agent.RunOutcome, 1)
	parentRunner := &fixedRunner{handle: &agent.RunHandle{Events: parentEvents, Done: parentDone}}
	childRunner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketDir, err := os.MkdirTemp("", "m09-disconnect-")
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	service, err := Serve(ctx, Deps{
		Store: db, Runner: parentRunner, ProjectRoot: root, SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
		Agents:    fixedTeamRoleCatalog{definition: agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}},
		Delegator: pool, ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v1",
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: "parent-disconnected", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Intent: "coordinate child work", Messages: []llm.Message{{Role: "user", Content: "coordinate child work"}},
	}
	t.Cleanup(func() {
		childRunner.release <- struct{}{}
		service.mu.Lock()
		stillActive := service.activeRuns[request.RunID] == session.ID
		service.mu.Unlock()
		if stillActive {
			parentEvents <- agent.ExecutionEvent{
				ID: "parent-disconnect-cleanup", RunID: request.RunID, SessionID: session.ID, RunSeq: 1,
				At: time.Now().UTC(), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`),
			}
			close(parentEvents)
			parentDone <- agent.RunOutcome{RunID: request.RunID, Status: agent.RunCompleted}
			close(parentDone)
		}
		_ = service.Close()
		pool.Close()
	})

	stream, err := OpenRun(ctx, service.deps.SocketPath, request)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.activeRuns[request.RunID] == session.ID
	})
	leadRequest, err := service.activeRunRequest(session.ID, request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	team, err := service.CreateTeam(t.Context(), leadRequest, "client-disconnect")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: "explore", Instruction: "Inspect the assigned area.", OriginCallID: "spawn-before-disconnect",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, childRunner.started)
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberRunning)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		for _, sub := range service.clients {
			if sub.runID == request.RunID {
				return false
			}
		}
		return true
	})
	service.mu.Lock()
	parentStillActive := service.activeRuns[request.RunID] == session.ID
	service.mu.Unlock()
	if !parentStillActive {
		t.Fatal("client socket disconnect canceled or removed the parent run")
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("client socket disconnect canceled accepted child: %v", child.ctx.Err())
	default:
	}

	parentEvents <- agent.ExecutionEvent{
		ID: "parent-disconnect-terminal", RunID: request.RunID, SessionID: session.ID, RunSeq: 1,
		At: time.Now().UTC(), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`),
	}
	close(parentEvents)
	parentDone <- agent.RunOutcome{RunID: request.RunID, Status: agent.RunCompleted}
	close(parentDone)
	service.mu.Lock()
	parentRunDone := service.runDone[request.RunID]
	service.mu.Unlock()
	if parentRunDone == nil {
		t.Fatal("parent run has no completion registration")
	}
	select {
	case <-parentRunDone:
	case <-time.After(3 * time.Second):
		t.Fatal("parent run did not finish after the client disconnected")
	}
	select {
	case <-child.ctx.Done():
		t.Fatalf("parent terminal after client disconnect canceled accepted child: %v", child.ctx.Err())
	default:
	}
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("member state after disconnected parent run terminal=%s, want running", got)
	}

	childRunner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, session.ID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[child.input.TeamTurn.TurnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("child turn after parent client disconnect=%s, want succeeded", got)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var parentTerminal bool
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == request.RunID && runEvent.Kind == string(agent.EventTerminal) {
			parentTerminal = true
		}
	}
	if !parentTerminal {
		t.Fatal("parent terminal event was not persisted after socket disconnect")
	}
}
