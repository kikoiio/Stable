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

func TestParentCancellationDoesNotCompleteTeamTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "cancel-task-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	childRunner := &cancelAwareTeamChildRunner{
		started: make(chan teamChildStart, 1), canceled: make(chan string, 1),
	}
	parentRunner := &recordingParentRunner{}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
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

	team, err := service.CreateTeam(t.Context(), request, "cancel-task-state")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area A.", OriginCallID: "spawn-task-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := receiveTeamChildStart(t, childRunner.started)
	if started.input.TeamTurn == nil || started.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected active child identity: %+v", started.input.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberRunning)

	workTask, err := service.CreateTeamTask(t.Context(), request, team.ID, teams.Task{
		Title: "inspect assigned area", Assignee: member.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := teams.TaskInProgress
	workTask, err = service.UpdateTeamTask(t.Context(), request, team.ID, workTask.ID, workTask.Revision, teams.TaskPatch{Status: &inProgress})
	if err != nil {
		t.Fatal(err)
	}
	dependentTask, err := service.CreateTeamTask(t.Context(), request, team.ID, teams.Task{
		Title: "review findings", BlockedBy: []string{workTask.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dependentTask.Status != teams.TaskBlocked {
		t.Fatalf("dependent task status=%s before cancel, want blocked", dependentTask.Status)
	}

	if err := service.cancelRun(ClientMsg{RunID: request.RunID, SessionID: request.Work.SessionID}, make(chan ServerMsg, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("parent cancellation did not cancel its team child")
	}
	select {
	case turnID := <-childRunner.canceled:
		if turnID != started.input.TeamTurn.TurnID {
			t.Fatalf("canceled turn=%s, want %s", turnID, started.input.TeamTurn.TurnID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("team child runner did not return after parent cancellation")
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberInterrupted)

	gotTask, err := service.GetTeamTask(t.Context(), request, team.ID, workTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotDependent, err := service.GetTeamTask(t.Context(), request, team.ID, dependentTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.Assignee != member.ID || gotTask.Status != teams.TaskInProgress || gotTask.Revision != workTask.Revision {
		t.Fatalf("parent cancellation changed the active task: %+v", gotTask)
	}
	if gotDependent.Status != teams.TaskBlocked || len(gotDependent.BlockedBy) != 1 || gotDependent.BlockedBy[0] != workTask.ID {
		t.Fatalf("parent cancellation released dependent task: %+v", gotDependent)
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable := projection.Tasks[workTask.ID]; durable.Assignee != member.ID || durable.Status != teams.TaskInProgress || durable.Revision != workTask.Revision {
		t.Fatalf("replay changed task state after parent cancellation: %+v", durable)
	}
	graph, err := teamTaskGraph(projection, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable, ok := graph.Get(dependentTask.ID); !ok || durable.Status != teams.TaskBlocked || len(durable.BlockedBy) != 1 || durable.BlockedBy[0] != workTask.ID {
		t.Fatalf("replay released dependent after parent cancellation: %+v (found=%v)", durable, ok)
	}
}
