package conversation

import (
	"context"
	"encoding/json"
	"errors"
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

func TestStalePlanRequestResponseLeavesRequestAndLogUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "stale-plan-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
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
	teamID, memberID, turnID := "", "", ""
	var memberRevision uint64
	t.Cleanup(func() {
		select {
		case runner.release <- struct{}{}:
		default:
		}
		if teamID != "" && memberID != "" && turnID != "" {
			waitForTeamTurnAndMemberRevision(t, root, request.Work.SessionID, teamID, memberID, turnID, memberRevision, string(agent.DelegationSucceeded))
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "stale-plan")
	if err != nil {
		t.Fatal(err)
	}
	teamID = team.ID
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the parser.", PlanRequired: true, OriginCallID: "spawn-stale-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberID = member.ID
	child := receiveTeamChildInput(t, runner.inputs)
	if child.TeamTurn == nil || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected plan member turn: %+v", child.TeamTurn)
	}
	turnID = child.TeamTurn.TurnID
	childRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: request.Work, TeamTurn: child.TeamTurn}
	pending, err := service.SubmitTeamPlan(t.Context(), childRequest, team.ID, "Inspect the parser and report findings.")
	if err != nil || pending.Status != teams.RequestPending || pending.Revision != 1 {
		t.Fatalf("plan submission=%+v err=%v; want pending revision 1", pending, err)
	}
	beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := beforeProjection.Requests[pending.ID]; got.Status != teams.RequestPending || got.Revision != pending.Revision {
		t.Fatalf("precondition request projection=%+v, want pending revision %d", got, pending.Revision)
	}
	beforeTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTeamHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision+1, string(teams.RequestApproved), "stale approval"); !errors.Is(err, teams.ErrRevisionConflict) {
		t.Fatalf("stale plan response error=%v, want revision conflict", err)
	}

	afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRequest := afterProjection.Requests[pending.ID]
	memberRevision = afterProjection.Members[member.ID].Revision
	if afterRequest.Status != teams.RequestPending || afterRequest.Revision != pending.Revision || afterRequest.ResponderID != pending.ResponderID || afterProjection.Members[member.ID].PlanApproved {
		t.Fatalf("stale response changed request/member state: request before=%+v after=%+v member=%+v", pending, afterRequest, afterProjection.Members[member.ID])
	}
	afterTranscript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatalf("stale response appended session events: %d -> %d", len(beforeTranscript.Events), len(afterTranscript.Events))
	}
	afterTeamHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTeamHistory) != len(beforeTeamHistory) {
		t.Fatalf("stale response appended team facts: %d -> %d", len(beforeTeamHistory), len(afterTeamHistory))
	}
	for _, event := range afterTeamHistory {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamRequestResponded && fact.Request != nil && fact.Request.ID == pending.ID {
			t.Fatal("stale response appended a response fact for the pending plan")
		}
	}
}

func waitForTeamTurnAndMemberRevision(t *testing.T, root, sessionID, teamID, memberID, turnID string, afterRevision uint64, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Turns[turnID].Status == status && projection.Members[memberID].Revision > afterRevision {
			return
		}
		time.Sleep(time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("turn status/revision=%s/%d, want %s/revision > %d", projection.Turns[turnID].Status, projection.Members[memberID].Revision, status, afterRevision)
}
