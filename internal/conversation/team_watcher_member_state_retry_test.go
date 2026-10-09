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

func TestTeamWatcherRetriesFinalMemberStateAppendAndClosesTeam(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "watcher-member-state-retry-parent")
	permissionBounds, err := json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	serviceCtx, cancelService := context.WithCancel(context.Background())
	service.lifeCtx = serviceCtx
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
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
	var memberStateAttempts atomic.Int32
	memberStateFailure := make(chan struct{}, 1)
	service.teamMemberStateAppender = func(appendRoot string, team teams.Team, runID, actor string, member teams.Member) error {
		if memberStateAttempts.Add(1) == 1 {
			memberStateFailure <- struct{}{}
			return errors.New("injected temporary member-state append failure")
		}
		return appendTeamFactLocked(appendRoot, team.Scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: actor, ActorRunID: runID, Member: &member})
	}
	cleaned := false
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		if !cleaned {
			cancelService()
			service.teamScheduler.close()
			pool.Close()
		}
	})

	team, err := service.CreateTeam(t.Context(), request, "watcher-member-state-retry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "spawn-member-state-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildStart(t, runner.started)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)
	closing, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("close active team = %+v, err=%v; want closing", closing, err)
	}
	runner.release <- struct{}{}
	select {
	case <-memberStateFailure:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exercise the injected final member-state append failure")
	}
	if got := watcherTeamTerminalCount(t, root, request.Work.SessionID, child.input.TeamTurn.TurnID); got != 1 {
		t.Fatalf("TeamTurnTerminal facts after member-state failure=%d, want one", got)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberStopped)
	waitForTeamClosed(t, root, request.Work.SessionID, team.ID)
	if got := memberStateAttempts.Load(); got != 2 {
		t.Fatalf("final member-state append attempts=%d, want one failed attempt and one retry", got)
	}
	assertWatcherTerminalFactCounts(t, root, request.Work.SessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, child.input.ChildRunID, teams.MemberStopped)
	if got := watcherMemberTerminalStateCount(t, root, request.Work.SessionID, team.ID, member.ID, child.input.TeamTurn.TurnID, teams.MemberStopped); got != 1 {
		t.Fatalf("persisted final member-state facts=%d, want exactly one", got)
	}
	select {
	case extra := <-runner.started:
		t.Fatalf("member-state retry reran child: %+v", extra.input)
	default:
	}

	cancelService()
	service.teamScheduler.close()
	pool.Close()
	cleaned = true
}

func waitForTeamClosed(t *testing.T, root, sessionID, teamID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Teams[teamID].Status == teams.TeamClosed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("team status=%s, want closed", projection.Teams[teamID].Status)
}

func watcherMemberTerminalStateCount(t *testing.T, root, sessionID, teamID, memberID, turnID string, status teams.MemberStatus) int {
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
		if decodeSessionData(event.Data, &fact) == nil && fact.TeamID == teamID && fact.Kind == sessionlog.TeamMemberState && fact.Member != nil && fact.Member.ID == memberID && fact.Member.TurnID == turnID && fact.Member.Status == status {
			count++
		}
	}
	return count
}
