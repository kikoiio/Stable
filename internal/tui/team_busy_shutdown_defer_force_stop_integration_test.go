package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// This combines the user-facing shutdown request/defer path with a real TUI
// force-stop of that same busy child and verifies that sibling/task facts stay
// isolated and durable through the child's actual exit.
func TestTeamTUIBusyShutdownDeferThenForceStopPreservesTasksAndSibling(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tui-shutdown-force-stop-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	childRunner := &shutdownForceStopChildRunner{started: make(chan shutdownForceStopChild, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 2, 2
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "conversation.sock")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Service.Close waits for child watchers, so release any remaining gated
		// runner before asking the service and shared pool to shut down.
		childRunner.releaseAll()
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "shutdown then force stop one busy child",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("start parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create defer-force-stop")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	members := make(map[string]shutdownForceStopChild, 2)
	memberIDsByName := make(map[string]string, 2)
	for _, name := range []string{"reader-target", "reader-sibling"} {
		_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn "+name+" explore inspect assigned work")
		spawned := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
		if spawned == nil {
			t.Fatalf("TUI spawn omitted member %s", name)
		}
		child := receiveShutdownForceStopChild(t, childRunner.started)
		if child.input.TeamTurn == nil || child.input.TeamTurn.MemberID != spawned.ID {
			t.Fatalf("runner received wrong child for %s: %+v", name, child.input.TeamTurn)
		}
		members[spawned.ID] = child
		memberIDsByName[name] = spawned.ID
	}
	targetID, siblingID := memberIDsByName["reader-target"], memberIDsByName["reader-sibling"]
	if targetID == "" || siblingID == "" || targetID == siblingID {
		t.Fatalf("spawned member identities target=%q sibling=%q", targetID, siblingID)
	}
	target, sibling := members[targetID], members[siblingID]

	// Create target, sibling, and dependent tasks through TUI/socket. Both
	// members claim their own task; the dependent remains blocked by target.
	_, taskAResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" tasks create target-work --assignee "+targetID)
	taskA := acceptanceTeamResponse(t, taskAResult, "team_task_create").TeamTask
	_, taskBResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" tasks create sibling-work --assignee "+siblingID)
	taskB := acceptanceTeamResponse(t, taskBResult, "team_task_create").TeamTask
	if taskA == nil || taskB == nil {
		t.Fatalf("task setup omitted a prerequisite task: A=%+v B=%+v", taskA, taskB)
	}
	_, taskCResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" tasks create target-dependent --assignee "+siblingID+" --blocked-by "+taskA.ID)
	taskC := acceptanceTeamResponse(t, taskCResult, "team_task_create").TeamTask
	if taskC == nil {
		t.Fatalf("task setup omitted dependent task C: %+v", taskC)
	}
	inProgress := string(teams.TaskInProgress)
	for _, claim := range []struct {
		child shutdownForceStopChild
		task  *teams.Task
		id    string
	}{{target, taskA, "target"}, {sibling, taskB, "sibling"}} {
		args, _ := json.Marshal(map[string]any{"team_id": team.ID, "task_id": claim.task.ID, "expected_revision": claim.task.Revision, "status": inProgress})
		outcome, callErr := svc.ExecuteTeamTool(reqctx, agent.ExecutionRequest{RunID: claim.child.input.ChildRunID, Work: claim.child.input.Work, TeamTurn: claim.child.input.TeamTurn}, llm.ToolUse{ID: "claim-" + claim.id, Name: "team_task_update", Arguments: args})
		if callErr != nil || outcome.Status != agent.ToolSucceeded || outcome.IsError {
			t.Fatalf("%s child claim outcome=%+v err=%v", claim.id, outcome, callErr)
		}
	}

	_, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+targetID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestPending || shutdown.MemberID != targetID {
		t.Fatalf("target shutdown request=%+v", shutdown)
	}
	deferArgs, _ := json.Marshal(map[string]any{"team_id": team.ID, "request_id": shutdown.ID, "expected_revision": shutdown.Revision, "decision": string(teams.RequestDeferred), "feedback": "Finish the current task before stopping."})
	deferred, err := svc.ExecuteTeamTool(reqctx, agent.ExecutionRequest{RunID: target.input.ChildRunID, Work: target.input.Work, TeamTurn: target.input.TeamTurn}, llm.ToolUse{ID: "defer-target-shutdown", Name: "team_request_respond", Arguments: deferArgs})
	if err != nil || deferred.Status != agent.ToolSucceeded || deferred.IsError {
		t.Fatalf("member defer outcome=%+v err=%v", deferred, err)
	}
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	var deferredListed bool
	for _, request := range acceptanceTeamResponse(t, listResult, "team_request_list").TeamRequests {
		if request.ID == shutdown.ID && request.Status == teams.RequestDeferred && request.Revision == 2 {
			deferredListed = true
		}
	}
	if !deferredListed {
		t.Fatal("TUI request list did not show the busy member's deferral")
	}

	_, stopResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" stop "+targetID)
	stopping := acceptanceTeamResponse(t, stopResult, "team_member_stop").TeamMember
	if stopping == nil || stopping.Status != teams.MemberStopping {
		t.Fatalf("TUI force-stop response=%+v, want stopping until actual child exit", stopping)
	}
	beforeRetry, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, retryStopResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" stop "+targetID)
	retriedStop := acceptanceTeamResponse(t, retryStopResult, "team_member_stop").TeamMember
	if retriedStop == nil || retriedStop.Status != teams.MemberStopping {
		t.Fatalf("repeated force-stop response=%+v, want idempotent stopping", retriedStop)
	}
	afterRetry, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRetry.Events) != len(beforeRetry.Events) {
		t.Fatalf("repeated force-stop appended durable facts: before=%d after=%d", len(beforeRetry.Events), len(afterRetry.Events))
	}
	select {
	case <-target.ctx.Done():
	case <-reqctx.Done():
		t.Fatal("TUI/socket force stop did not cancel the target child")
	}
	select {
	case <-sibling.ctx.Done():
		t.Fatal("force-stopping target also canceled the sibling")
	default:
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[targetID].Status != teams.MemberStopping || projection.Members[siblingID].Status != teams.MemberRunning {
		t.Fatalf("pre-exit member states target=%+v sibling=%+v", projection.Members[targetID], projection.Members[siblingID])
	}
	if request := projection.Requests[shutdown.ID]; request.Status != teams.RequestApproved || request.Revision != 3 || request.Feedback != "User force-stopped this member." {
		t.Fatalf("force-stop did not promote the deferred shutdown request: %+v", request)
	}
	assertShutdownForceStopTasks(t, projection.Tasks, team.ID, taskA.ID, taskB.ID, taskC.ID, targetID, siblingID)

	target.release()
	waitForForceStopTeamStatus(t, project, sessionID, team.ID, targetID, teams.MemberStopped)
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[targetID].Status != teams.MemberStopped || projection.Members[siblingID].Status != teams.MemberRunning {
		t.Fatalf("post-exit member states target=%+v sibling=%+v", projection.Members[targetID], projection.Members[siblingID])
	}
	if got := projection.Turns[target.input.TeamTurn.TurnID]; got.Status != string(agent.DelegationCanceled) {
		t.Fatalf("target turn after actual cancellation exit=%+v", got)
	}
	if got := projection.Turns[sibling.input.TeamTurn.TurnID]; got.Status == string(agent.DelegationCanceled) || got.Status == string(agent.DelegationSucceeded) || got.Status == string(agent.DelegationFailed) {
		t.Fatalf("sibling turn changed before its runner was released: %+v", got)
	}
	if request := projection.Requests[shutdown.ID]; request.Status != teams.RequestApproved || request.Revision != 3 || request.Feedback != "User force-stopped this member." {
		t.Fatalf("target force-stop request after child exit=%+v", request)
	}
	assertShutdownForceStopTasks(t, projection.Tasks, team.ID, taskA.ID, taskB.ID, taskC.ID, targetID, siblingID)
}

func assertShutdownForceStopTasks(t *testing.T, tasks map[string]teams.Task, teamID, targetTaskID, siblingTaskID, dependentTaskID, targetID, siblingID string) {
	t.Helper()
	targetTask, siblingTask, dependent := tasks[targetTaskID], tasks[siblingTaskID], tasks[dependentTaskID]
	if targetTask.Assignee != targetID || targetTask.Status != teams.TaskInProgress {
		t.Fatalf("force stop incorrectly completed or reassigned target task: %+v", targetTask)
	}
	if siblingTask.Assignee != siblingID || siblingTask.Status != teams.TaskInProgress {
		t.Fatalf("target force stop changed sibling task: %+v", siblingTask)
	}
	graphTasks := make([]teams.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.TeamID == teamID {
			graphTasks = append(graphTasks, task)
		}
	}
	graph, err := teams.LoadTaskGraph(teamID, graphTasks)
	if err != nil {
		t.Fatalf("rebuild task graph after force stop: %v", err)
	}
	projectedDependent, ok := graph.Get(dependentTaskID)
	if !ok || dependent.Assignee != siblingID || dependent.Status != teams.TaskPending || projectedDependent.Status != teams.TaskBlocked || len(dependent.BlockedBy) != 1 || dependent.BlockedBy[0] != targetTaskID {
		t.Fatalf("target force stop changed dependent task relationship: %+v", dependent)
	}
}

type shutdownForceStopChild struct {
	input   agent.ChildRunInput
	ctx     context.Context
	gate    chan struct{}
	gateOne *sync.Once
}

func (c *shutdownForceStopChild) release() { c.gateOne.Do(func() { close(c.gate) }) }

type shutdownForceStopChildRunner struct {
	started chan shutdownForceStopChild
	mu      sync.Mutex
	active  []*shutdownForceStopChild
}

func (r *shutdownForceStopChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	child := &shutdownForceStopChild{input: input, ctx: ctx, gate: make(chan struct{}), gateOne: &sync.Once{}}
	r.mu.Lock()
	r.active = append(r.active, child)
	r.mu.Unlock()
	select {
	case r.started <- *child:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	<-child.gate
	if ctx.Err() != nil {
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: "fixture observed user force stop"}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "sibling task remains active"}
}

func (r *shutdownForceStopChildRunner) releaseAll() {
	r.mu.Lock()
	active := append([]*shutdownForceStopChild(nil), r.active...)
	r.mu.Unlock()
	for _, child := range active {
		child.release()
	}
}

func receiveShutdownForceStopChild(t *testing.T, started <-chan shutdownForceStopChild) shutdownForceStopChild {
	t.Helper()
	select {
	case child := <-started:
		return child
	case <-time.After(5 * time.Second):
		t.Fatal("team child did not reach its running gate")
		return shutdownForceStopChild{}
	}
}
