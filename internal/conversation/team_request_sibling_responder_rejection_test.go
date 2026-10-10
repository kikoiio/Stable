package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestSiblingMemberCannotRespondToAnotherMembersShutdownRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "sibling-shutdown-parent")
	leadRequest.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: leadRequest.RunID, SessionID: leadRequest.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{leadRequest.RunID: leadRequest}
	role := agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit",
		Tools: []string{"read_file"}, MaxTurns: 3,
	}
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
		for range 2 {
			select {
			case runner.release <- struct{}{}:
			default:
			}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), leadRequest, "sibling-shutdown")
	if err != nil {
		t.Fatal(err)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-a", AgentName: role.Name, Instruction: "Inspect area A.", OriginCallID: "spawn-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	childA := receiveTeamChildInput(t, runner.inputs)
	if childA.TeamTurn == nil || childA.TeamTurn.MemberID != memberA.ID {
		t.Fatalf("first child turn=%+v, want member %s", childA.TeamTurn, memberA.ID)
	}

	memberB, err := service.SpawnTeamMember(t.Context(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-b", AgentName: role.Name, Instruction: "Inspect area B.", OriginCallID: "spawn-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	childB := receiveTeamChildInput(t, runner.inputs)
	if childB.TeamTurn == nil || childB.TeamTurn.MemberID != memberB.ID {
		t.Fatalf("second child turn=%+v, want member %s", childB.TeamTurn, memberB.ID)
	}
	requestB := agent.ExecutionRequest{RunID: childB.ChildRunID, Work: childB.Work, TeamTurn: childB.TeamTurn}

	shutdown, err := service.RequestTeamShutdown(t.Context(), leadRequest, team.ID, memberA.ID)
	if err != nil || shutdown.Status != teams.RequestPending {
		t.Fatalf("busy shutdown request=%+v err=%v; want pending", shutdown, err)
	}
	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	transcriptBefore, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.RespondTeamRequest(t.Context(), requestB, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), "Please let A finish."); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("sibling member response error=%v, want permission denied", err)
	}

	after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected sibling response changed team projection: before=%+v after=%+v", before, after)
	}
	stored := after.Requests[shutdown.ID]
	if stored.Status != teams.RequestPending || stored.Revision != shutdown.Revision || stored.ResponderID != shutdown.ResponderID || stored.MemberID != memberA.ID {
		t.Fatalf("sibling response changed shutdown request: %+v", stored)
	}
	if after.Members[memberA.ID].Status != teams.MemberRunning || after.Members[memberB.ID].Status != teams.MemberRunning {
		t.Fatalf("rejected sibling response changed member status: A=%s B=%s", after.Members[memberA.ID].Status, after.Members[memberB.ID].Status)
	}
	historyAfter, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfter) != len(historyBefore) {
		t.Fatalf("rejected sibling response changed team history length: before=%d after=%d", len(historyBefore), len(historyAfter))
	}
	transcriptAfter, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcriptAfter.Events) != len(transcriptBefore.Events) {
		t.Fatalf("rejected sibling response appended session events: before=%d after=%d", len(transcriptBefore.Events), len(transcriptAfter.Events))
	}
}
