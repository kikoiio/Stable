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
	"stable/internal/teams"
)

func TestStopTeamMemberRemainsStoppingUntilChildExits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stop-parent")
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
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "stop-until-exit")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "call-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveTeamChildInput(t, runner.inputs)
	if input.TeamTurn == nil || input.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected child turn: %+v", input.TeamTurn)
	}

	stopping, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopping.Status != teams.MemberStopping || stopping.TurnID != input.TeamTurn.TurnID {
		t.Fatalf("stop returned member %+v; want stopping turn %s", stopping, input.TeamTurn.TurnID)
	}
	assertTeamStopFacts(t, root, request.Work.SessionID, team.ID, member.ID, input.TeamTurn.TurnID, false)

	_, _ = service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, member.ID)
	assertTeamStopFacts(t, root, request.Work.SessionID, team.ID, member.ID, input.TeamTurn.TurnID, false)

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberStopped)
	assertTeamStopFacts(t, root, request.Work.SessionID, team.ID, member.ID, input.TeamTurn.TurnID, true)
}

func TestCloseTeamWaitsForActualChildExit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "close-parent")
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
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "close-until-exit")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "call-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveTeamChildInput(t, runner.inputs)
	closing, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closing.Status != teams.TeamClosing {
		t.Fatalf("close while child active = %+v, err=%v; want closing", closing, err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Teams[team.ID].Status != teams.TeamClosing || projection.Members[member.ID].Status.IsTerminal() || projection.Turns[input.TeamTurn.TurnID].Status == "succeeded" || projection.Turns[input.TeamTurn.TurnID].Status == "canceled" {
		t.Fatalf("team close wrote terminal facts before child exit: team=%+v member=%+v turn=%+v", projection.Teams[team.ID], projection.Members[member.ID], projection.Turns[input.TeamTurn.TurnID])
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberStopped)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Teams[team.ID].Status == teams.TeamClosed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if projection.Teams[team.ID].Status != teams.TeamClosed {
		t.Fatalf("team status after child exit = %s, want closed", projection.Teams[team.ID].Status)
	}
	if runner.childCount() != 1 {
		t.Fatalf("closing team started %d child turns, want one", runner.childCount())
	}
}

func assertTeamStopFacts(t *testing.T, root, sessionID, teamID, memberID, turnID string, wantTerminal bool) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != teamID {
		t.Fatalf("member %s missing from team projection", memberID)
	}
	if !wantTerminal && (member.Status.IsTerminal() || member.Status != teams.MemberStopping) {
		t.Fatalf("member became terminal before child exit: %s", member.Status)
	}
	if wantTerminal && member.Status != teams.MemberStopped {
		t.Fatalf("member status after child exit = %s, want stopped", member.Status)
	}

	shutdownRequests := 0
	for _, request := range projection.Requests {
		if request.MemberID == memberID && request.Type == teams.RequestShutdown {
			shutdownRequests++
			if request.Status != teams.RequestApproved {
				t.Fatalf("shutdown request status = %s, want approved", request.Status)
			}
		}
	}
	if shutdownRequests != 1 {
		t.Fatalf("persisted shutdown requests = %d, want exactly one", shutdownRequests)
	}

	facts, err := sessionlog.TeamHistory(root, sessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	terminalFacts := 0
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turnID {
			terminalFacts++
		}
	}
	want := 0
	if wantTerminal {
		want = 1
	}
	if terminalFacts != want {
		t.Fatalf("turn %s terminal facts = %d, want %d", turnID, terminalFacts, want)
	}
}
