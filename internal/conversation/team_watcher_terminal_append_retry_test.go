package conversation

import (
	"context"
	"encoding/json"
	"errors"
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

func TestTeamWatcherRetriesTerminalAppendAndRecoveryIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "watcher-terminal-retry-parent")
	permissionBounds, err := json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	serviceCtx, cancelService := context.WithCancel(context.Background())
	service.lifeCtx = serviceCtx
	role := agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3,
	}
	runner := &terminalIndependentTeamChildRunner{started: make(chan teamChildStart, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		cancelService()
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.teamScheduler = newTeamScheduler(service)
	firstServiceStopped := false
	var terminalAppendCalls atomic.Int32
	firstAppendFailed := make(chan struct{}, 1)
	service.teamScheduler.appendTurnTerminal = func(appendRoot, sessionID, teamID string, fact sessionlog.TeamEvent) error {
		if terminalAppendCalls.Add(1) == 1 {
			firstAppendFailed <- struct{}{}
			return errors.New("injected temporary TeamTurnTerminal append failure")
		}
		return appendTeamFactLocked(appendRoot, sessionID, teamID, fact)
	}
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		if !firstServiceStopped {
			cancelService()
			service.teamScheduler.close()
			pool.Close()
		}
	})

	team, err := service.CreateTeam(t.Context(), request, "watcher-terminal-retry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "spawn-watcher-terminal-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)
	runner.release <- struct{}{}
	select {
	case <-firstAppendFailed:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exercise the injected TeamTurnTerminal append failure")
	}
	failedProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedProjection.Turns[child.input.TeamTurn.TurnID].Status != "queued" || failedProjection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("failed terminal append published false terminal state: turn=%s member=%s", failedProjection.Turns[child.input.TeamTurn.TurnID].Status, failedProjection.Members[member.ID].Status)
	}
	if got := watcherTeamTerminalCount(t, root, request.Work.SessionID, child.input.TeamTurn.TurnID); got != 0 {
		t.Fatalf("failed terminal append persisted %d TeamTurnTerminal facts, want zero", got)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if got := terminalAppendCalls.Load(); got != 2 {
		t.Fatalf("TeamTurnTerminal append attempts=%d, want one failed attempt followed by one successful retry", got)
	}
	if child.input.ChildRunID == "" {
		t.Fatal("fixture child has no durable run identity")
	}
	assertWatcherTerminalFactCounts(t, root, request.Work.SessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, child.input.ChildRunID, teams.MemberIdle)
	select {
	case extra := <-runner.started:
		t.Fatalf("watcher retry reran child: %+v", extra.input)
	default:
	}

	// Stop the first service after its watcher has completed, then let a real
	// Service startup replay the already-completed journal. Recovery must not
	// submit the child again or append duplicate terminal facts.
	cancelService()
	service.teamScheduler.close()
	pool.Close()
	firstServiceStopped = true
	socketDir, err := os.MkdirTemp(".tmp", "m09-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	recoveryRunner := &recoveryMustNotRunTeamChild{started: make(chan agent.ChildRunInput, 1)}
	recoveryPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), recoveryRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	recoveryCtx, cancelRecovery := context.WithCancel(context.Background())
	restarted, err := Serve(recoveryCtx, Deps{ProjectRoot: root, SocketPath: filepath.Join(socketDir, "s.sock"), Delegator: recoveryPool})
	if err != nil {
		cancelRecovery()
		recoveryPool.Close()
		t.Fatalf("start service and recover watcher terminal: %v", err)
	}
	t.Cleanup(func() {
		cancelRecovery()
		if err := restarted.Close(); err != nil {
			t.Errorf("close recovery service: %v", err)
		}
		recoveryPool.Close()
	})
	assertWatcherTerminalFactCounts(t, root, request.Work.SessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, child.input.ChildRunID, teams.MemberInterrupted)
	select {
	case input := <-recoveryRunner.started:
		t.Fatalf("startup recovery reran child: %+v", input)
	default:
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != beforeEvents {
		t.Fatalf("repeated recovery changed event count: %d -> %d", beforeEvents, len(transcript.Events))
	}
	assertWatcherTerminalFactCounts(t, root, request.Work.SessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, child.input.ChildRunID, teams.MemberInterrupted)
}

func assertWatcherTerminalFactCounts(t *testing.T, root, sessionID, teamID, memberID, turnID, childRunID string, wantMemberStatus teams.MemberStatus) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turnID].Status; got != string(agent.DelegationSucceeded) {
		t.Fatalf("turn status=%q, want succeeded", got)
	}
	if got := projection.Members[memberID].Status; got != wantMemberStatus {
		t.Fatalf("member status=%q, want %s", got, wantMemberStatus)
	}
	teamTerminals, runTerminals := watcherTeamTerminalCount(t, root, sessionID, turnID), 0
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventRunEvent:
			var runEvent sessionlog.RunEvent
			if err := decodeSessionData(event.Data, &runEvent); err != nil {
				t.Fatal(err)
			}
			if runEvent.RunID == childRunID && runEvent.Kind == string(agent.EventTerminal) {
				runTerminals++
			}
		}
	}
	if teamTerminals != 1 || runTerminals != 1 {
		t.Fatalf("terminal counts: team=%d run=%d, want exactly one each", teamTerminals, runTerminals)
	}
	if _, err := sessionlog.TeamHistory(root, sessionID, teamID, 0, teams.MaxPageSize); err != nil {
		t.Fatalf("read team history after watcher retry: %v", err)
	}
}

func watcherTeamTerminalCount(t *testing.T, root, sessionID, turnID string) int {
	t.Helper()
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turnID {
			count++
		}
	}
	return count
}
