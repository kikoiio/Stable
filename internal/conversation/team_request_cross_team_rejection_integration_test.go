package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestPlanRequestCannotBeAnsweredThroughAnotherTeam(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "request-cross-team-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
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
		select {
		case runner.release <- struct{}{}:
		default:
		}
		service.teamScheduler.close()
		pool.Close()
	})

	requestTeam, err := service.CreateTeam(t.Context(), request, "request-owner")
	if err != nil {
		t.Fatal(err)
	}
	otherTeam, err := service.CreateTeam(t.Context(), request, "other-team")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: requestTeam.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the parser.", PlanRequired: true, OriginCallID: "spawn-plan-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	if child.TeamTurn == nil || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected plan member turn: %+v", child.TeamTurn)
	}
	childRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: request.Work, TeamTurn: child.TeamTurn}
	pending, err := service.SubmitTeamPlan(t.Context(), childRequest, requestTeam.ID, "Inspect parser recovery and report the result.")
	if err != nil || pending.Status != teams.RequestPending || pending.Revision != 1 {
		t.Fatalf("plan submission=%+v err=%v; want pending revision 1", pending, err)
	}

	if _, err := service.RespondTeamRequest(t.Context(), request, otherTeam.ID, pending.ID, pending.Revision, string(teams.RequestApproved), "cross-team approval"); !errors.Is(err, teams.ErrRevisionConflict) {
		t.Fatalf("cross-team response error=%v, want request not found/revision conflict", err)
	}
	requestProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, requestTeam.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := requestProjection.Requests[pending.ID]
	if after.Status != teams.RequestPending || after.Revision != pending.Revision || after.ResponderID != teams.Lead {
		t.Fatalf("cross-team answer mutated original request: before=%+v after=%+v", pending, after)
	}
	otherProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, otherTeam.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := otherProjection.Requests[pending.ID]; exists || len(otherProjection.Requests) != 0 {
		t.Fatalf("cross-team request leaked into other team's projection: %+v", otherProjection.Requests)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamRequestResponded && fact.Request != nil && fact.Request.ID == pending.ID {
			t.Fatal("cross-team response appended a response fact for the original request")
		}
	}
}
