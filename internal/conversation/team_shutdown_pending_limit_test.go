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

func TestTeamMemberCannotReceiveSecondUnresolvedShutdownRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "single-pending-shutdown-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	role := agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3,
	}
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

	team, err := service.CreateTeam(t.Context(), request, "single-pending-shutdown")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "single-pending-shutdown-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	childRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: request.Work, TeamTurn: child.TeamTurn}
	first, err := service.RequestTeamShutdown(t.Context(), request, team.ID, member.ID)
	if err != nil || first.Status != teams.RequestPending {
		t.Fatalf("first busy shutdown request = %+v, err=%v", first, err)
	}
	assertSecondShutdownRequestRejected := func(label string) {
		t.Helper()
		beforeProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		beforeHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
		if err != nil {
			t.Fatal(err)
		}
		beforeSession, err := sessionlog.Replay(root, request.Work.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.RequestTeamShutdown(t.Context(), request, team.ID, member.ID); !errors.Is(err, teams.ErrCapacity) {
			t.Fatalf("second shutdown request while %s = %v, want ErrCapacity", label, err)
		}
		afterProjection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(afterProjection, beforeProjection) {
			t.Fatalf("rejected shutdown while %s changed projection: before=%+v after=%+v", label, beforeProjection, afterProjection)
		}
		afterHistory, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
		if err != nil {
			t.Fatal(err)
		}
		afterSession, err := sessionlog.Replay(root, request.Work.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(afterHistory) != len(beforeHistory) || len(afterSession.Events) != len(beforeSession.Events) || runner.childCount() != 1 {
			t.Fatalf("rejected shutdown while %s changed durable facts or started child: history %d -> %d, events %d -> %d, children=%d", label, len(beforeHistory), len(afterHistory), len(beforeSession.Events), len(afterSession.Events), runner.childCount())
		}
	}
	assertSecondShutdownRequestRejected("pending")
	deferred, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, first.ID, first.Revision, string(teams.RequestDeferred), "Finish the current inspection first.")
	if err != nil || deferred.Status != teams.RequestDeferred {
		t.Fatalf("member deferred shutdown = %+v, err=%v", deferred, err)
	}
	assertSecondShutdownRequestRejected("deferred")
	rejected, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, first.ID, deferred.Revision, string(teams.RequestRejected), "Continue the assigned work.")
	if err != nil || rejected.Status != teams.RequestRejected {
		t.Fatalf("member rejected shutdown = %+v, err=%v", rejected, err)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	terminalPrior, err := service.RequestTeamShutdown(t.Context(), request, team.ID, member.ID)
	if err != nil || terminalPrior.Status != teams.RequestApproved || terminalPrior.ID == first.ID {
		t.Fatalf("terminal prior request blocked a new idle shutdown: %+v, err=%v", terminalPrior, err)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberStopped)
}
