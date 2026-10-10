package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

type crossGoalStopRunner struct {
	started  chan agent.ChildRunInput
	canceled chan struct{}
}

func (r *crossGoalStopRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	<-ctx.Done()
	close(r.canceled)
	return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: "stopped by user"}
}

func TestTeamStopSocketRejectsValidAnotherGoalOwnerIdentityWithoutFacts(t *testing.T) {
	project := t.TempDir()
	goalRootA := filepath.Join(project, "goal-a")
	goalRootB := filepath.Join(project, "goal-b")
	for _, root := range []string{goalRootA, goalRootB} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socketDir, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "cross-goal-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketDir, err = filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	runner := &crossGoalStopRunner{started: make(chan agent.ChildRunInput, 1), canceled: make(chan struct{})}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	role := agentcatalog.Definition{
		Name: "cross-goal-reader", Instruction: "Inspect the assigned area.", Model: "inherit",
		Tools: []string{"read_file"}, MaxTurns: 1,
	}
	service, err := Serve(ctx, Deps{
		ProjectRoot: project, Store: db, SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
		Delegator: pool, Agents: fixedTeamRoleCatalog{definition: role}, ForkProvider: forkSkillFixtureProvider{},
		ForkExecutorFactory: forkSkillFixtureExecutorFactory{}, ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}},
		ProviderName: "fixture", Model: "fixture-model",
	})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	t.Cleanup(func() {
		pool.Close()
	})
	requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
	defer requestCancel()

	created, err := Request(requestCtx, service.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	const goalIDA, goalIDB = "cross-goal-stop-a", "cross-goal-stop-b"
	for _, goal := range []struct{ id, root string }{{goalIDA, goalRootA}, {goalIDB, goalRootB}} {
		if _, err := db.CreateGoal(requestCtx, coreGoal(goal.id, goal.root, sessionID)); err != nil {
			t.Fatal(err)
		}
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalIDA, WorkItemID: "goal-a-item"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalIDB, WorkItemID: "goal-b-item"}
	makeLead := func(runID string, work agent.WorkRef, allowedRoot string) agent.ExecutionRequest {
		t.Helper()
		if _, err := sessionlog.Append(project, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "cross-Goal team stop identity",
		}); err != nil {
			t.Fatal(err)
		}
		bounds, err := json.Marshal(permission.Authority{
			RunID: runID, SessionID: sessionID, GoalID: work.GoalID, WorkItemID: work.WorkItemID, AllowedRoot: allowedRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		return agent.ExecutionRequest{RunID: runID, Work: work, PermissionBounds: bounds}
	}
	requestAOwner := makeLead("cross-goal-owner-a-run", workA, goalRootA)
	requestBOwner := makeLead("cross-goal-owner-b-run", workB, goalRootB)
	service.mu.Lock()
	for _, request := range []agent.ExecutionRequest{requestAOwner, requestBOwner} {
		service.activeRuns[request.RunID] = sessionID
		service.activeRequests[request.RunID] = request
	}
	service.mu.Unlock()
	if _, scope, actor, err := service.teamOperationScope(requestCtx, requestBOwner); err != nil || !actor.Lead || scope.GoalID != goalIDB || scope.WorkItemID != workB.WorkItemID || scope.ProjectRoot != goalRootB {
		t.Fatalf("Goal B actor is not a valid owner for its own scope: scope=%+v actor=%+v err=%v", scope, actor, err)
	}

	team, err := service.CreateTeam(requestCtx, requestAOwner, "goal-a-team")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(requestCtx, requestAOwner, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the assigned area.", OriginCallID: "cross-goal-stop-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-runner.started:
		if input.TeamTurn == nil || input.TeamTurn.MemberID != member.ID {
			t.Fatalf("started child turn=%+v, want member %s", input.TeamTurn, member.ID)
		}
	case <-requestCtx.Done():
		t.Fatal("team member did not start")
	}

	beforeProjection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(project, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Stop is a session-user operation. Supplying Goal B's otherwise valid
	// owner identity must be rejected at the socket boundary before dispatch.
	if _, err := Request(requestCtx, service.deps.SocketPath, ClientMsg{
		Op: "team_member_stop", SessionID: sessionID, TeamID: team.ID, TeamMemberID: member.ID,
		RunID: requestBOwner.RunID, GoalID: goalIDB, WorkItemID: workB.WorkItemID,
	}); err == nil {
		t.Fatal("socket accepted Goal B owner identity for Goal A member stop")
	}
	afterRejectedProjection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedHistory, err := sessionlog.TeamHistory(project, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterRejectedSession, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterRejectedProjection, beforeProjection) || !reflect.DeepEqual(afterRejectedHistory, beforeHistory) || !reflect.DeepEqual(afterRejectedSession.Events, beforeSession.Events) {
		t.Fatal("rejected cross-Goal stop changed projection, history, or session facts")
	}
	if got := afterRejectedProjection.Members[member.ID].Status; got != teams.MemberRunning {
		t.Fatalf("rejected cross-Goal stop changed member status to %s", got)
	}
	select {
	case <-runner.canceled:
		t.Fatal("rejected cross-Goal stop canceled the child")
	default:
	}

	response, err := Request(requestCtx, service.deps.SocketPath, ClientMsg{
		Op: "team_member_stop", SessionID: sessionID, TeamID: team.ID, TeamMemberID: member.ID,
	})
	if err != nil || len(response) != 1 || response[0].TeamMember == nil || response[0].TeamMember.Status != teams.MemberStopping {
		t.Fatalf("authorized session-user stop = %+v, %v; want member stopping", response, err)
	}
	select {
	case <-runner.canceled:
	case <-requestCtx.Done():
		t.Fatal("authorized Goal A team stop did not cancel its child")
	}
	waitForTeamMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberStopped)
	finalProjection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := finalProjection.Members[member.ID].Status; got != teams.MemberStopped {
		t.Fatalf("authorized stop final member status=%s, want stopped", got)
	}
}
