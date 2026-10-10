package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// This user path exercises capacity and budget state through the actual TUI
// command parser and conversation socket, while the shared pool is gated.
func TestTeamTUICapacityQuotaAndCumulativeBudgetScenario(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "team-capacity-scenario-")
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

	childRunner := &teamCapacityScenarioRunner{started: make(chan agent.ChildRunInput, 32), release: make(chan struct{}, 32), finished: make(chan struct{}, 32)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 32 {
			select {
			case childRunner.release <- struct{}{}:
			default:
			}
		}
		pool.Close()
	})
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
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, reqcancel := context.WithTimeout(ctx, 10*time.Second)
	defer reqcancel()
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "capacity scenario lead",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if msg, err := parentStream.Receive(); err != nil || msg.Type != "run_started" {
		t.Fatalf("start parent run: message=%+v err=%v", msg, err)
	}
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, createdTeam := submitAcceptanceTeamCommand(t, model, "/teams create capacity-scenario")
	team := acceptanceTeamResponse(t, createdTeam, "team_create").Team
	if team == nil {
		t.Fatal("team create omitted team")
	}
	permissionBounds, err := json.Marshal(permission.Authority{RunID: parentRunID, SessionID: sessionID, AllowedRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	parent := agent.ParentRun{RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, ProjectRoot: project, Provider: acceptanceTeamProvider{}, Model: "fixture-model", PermissionBounds: permissionBounds}

	// Saturate the only worker and queue slot. A first member spawn must fail
	// cleanly through TUI/socket and leave no member fact behind.
	blocker, err := pool.SubmitTask(reqctx, parent, agent.DelegationTask{ID: "scenario-pool-running", Name: "pool-running", Instruction: "hold worker"})
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveTeamCapacityScenarioInput(t, childRunner.started); got.Task.ID != blocker.TaskID {
		t.Fatalf("running blocker task=%q, want %q", got.Task.ID, blocker.TaskID)
	}
	queued, err := pool.SubmitTask(reqctx, parent, agent.DelegationTask{ID: "scenario-pool-queued", Name: "pool-queued", Instruction: "hold queue"})
	if err != nil {
		t.Fatal(err)
	}
	model, queueFull := submitTeamCapacityScenarioCommand(t, model, "/team "+team.ID+" spawn rejected explore must not start")
	if !errors.Is(queueFull.err, agent.ErrDelegationQueueFull) && (queueFull.err == nil || !strings.Contains(strings.ToLower(queueFull.err.Error()), "queue")) {
		t.Fatalf("full-pool spawn error=%v, want queue-full", queueFull.err)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil || len(projection.Members) != 0 {
		t.Fatalf("queue-full spawn persisted members: %+v err=%v", projection.Members, err)
	}
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)
	if got := receiveTeamCapacityScenarioInput(t, childRunner.started); got.Task.ID != queued.TaskID {
		t.Fatalf("queued pool task=%q, want %q", got.Task.ID, queued.TaskID)
	}
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)

	model, spawnA := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader-a explore inspect area a")
	memberA := acceptanceTeamResponse(t, spawnA, "team_member_spawn").TeamMember
	if memberA == nil {
		t.Fatal("member A spawn omitted member")
	}
	if got := receiveTeamCapacityScenarioInput(t, childRunner.started); got.TeamTurn == nil || got.TeamTurn.MemberID != memberA.ID {
		t.Fatalf("member A initial turn=%+v", got.TeamTurn)
	}
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)
	waitTeamCapacityScenarioMemberStatus(t, project, sessionID, team.ID, memberA.ID, teams.MemberIdle)
	model, spawnB := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader-b explore inspect area b")
	memberB := acceptanceTeamResponse(t, spawnB, "team_member_spawn").TeamMember
	if memberB == nil {
		t.Fatal("member B spawn omitted member")
	}
	if got := receiveTeamCapacityScenarioInput(t, childRunner.started); got.TeamTurn == nil || got.TeamTurn.MemberID != memberB.ID {
		t.Fatalf("member B initial turn=%+v", got.TeamTurn)
	}
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)
	waitTeamCapacityScenarioMemberStatus(t, project, sessionID, team.ID, memberB.ID, teams.MemberIdle)

	// Persist a historical member fact without passing through SpawnTeamMember,
	// so this identity has no in-memory lead grant. A pending message alone must
	// not make a later pool-capacity notification authorize a child run.
	ungrantedID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	ungranted := teams.Member{ID: ungrantedID, TeamID: team.ID, Name: "ungranted", AgentName: "explore", RoleHash: "fixture-role", Model: "fixture-model", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	factID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: factID, TeamID: team.ID, SessionID: sessionID, Kind: sessionlog.TeamMemberAdded,
		Revision: current.Teams[team.ID].Revision + 1, ActorID: teams.Lead, ActorRunID: parentRunID, Member: &ungranted,
	}); err != nil {
		t.Fatal(err)
	}
	model, ungrantedMessage := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+ungrantedID+" no scheduler grant")
	if acceptanceTeamResponse(t, ungrantedMessage, "team_send").TeamMessage == nil {
		t.Fatal("pending message for ungranted persisted member was not accepted")
	}

	// Refill the pool, enqueue exactly the recipient's quota, and verify the
	// next send is rejected without consuming a token or changing the count.
	blocker, err = pool.SubmitTask(reqctx, parent, agent.DelegationTask{ID: "scenario-message-running", Name: "message-running", Instruction: "hold worker"})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveTeamCapacityScenarioInput(t, childRunner.started)
	queued, err = pool.SubmitTask(reqctx, parent, agent.DelegationTask{ID: "scenario-message-queued", Name: "message-queued", Instruction: "hold queue"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range teams.MaxRecipientPending {
		model, result := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+memberA.ID+" pending message "+strconv.Itoa(i))
		if response := acceptanceTeamResponse(t, result, "team_send"); response.TeamMessage == nil {
			t.Fatalf("pending message %d omitted durable send response", i)
		}
		_ = model
	}
	waitTeamCapacityScenarioMemberStatus(t, project, sessionID, team.ID, memberA.ID, teams.MemberWaitingCapacity)
	before, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil || len(before.Messages) != teams.MaxRecipientPending+1 {
		t.Fatalf("pending boundary=%d err=%v, want %d A deliveries plus one ungranted delivery", len(before.Messages), err, teams.MaxRecipientPending)
	}
	model, rejectedMessage := submitTeamCapacityScenarioCommand(t, model, "/team "+team.ID+" send "+memberA.ID+" one over the limit")
	if !errors.Is(rejectedMessage.err, teams.ErrCapacity) && (rejectedMessage.err == nil || !strings.Contains(strings.ToLower(rejectedMessage.err.Error()), "capacity")) {
		t.Fatalf("over-limit TUI message error=%v, want capacity error", rejectedMessage.err)
	}
	after, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil || len(after.Messages) != len(before.Messages) {
		t.Fatalf("rejected pending message changed delivery count: before=%d after=%d err=%v", len(before.Messages), len(after.Messages), err)
	}

	// The capacity signal retries the authorized waiter with its persisted
	// backlog. The other, authorized but idle member must not receive a turn.
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)
	if got := receiveTeamCapacityScenarioInput(t, childRunner.started); got.Task.ID != queued.TaskID {
		t.Fatalf("pool queue order changed while releasing capacity: %q", got.Task.ID)
	}
	childRunner.release <- struct{}{}
	waitTeamCapacityScenarioFinished(t, childRunner)
	firstWaiter := receiveTeamCapacityScenarioInput(t, childRunner.started)
	if firstWaiter.TeamTurn == nil || firstWaiter.TeamTurn.MemberID != memberA.ID || !strings.Contains(firstWaiter.Task.Instruction, "pending message 0") {
		t.Fatalf("capacity release did not wake A with its pending message: %+v", firstWaiter)
	}
	select {
	case other := <-childRunner.started:
		t.Fatalf("capacity release spuriously started another member: %+v", other.TeamTurn)
	case <-time.After(40 * time.Millisecond):
	}
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[ungrantedID].Status != teams.MemberCreated {
		t.Fatalf("capacity signal changed ungranted member state: %+v", projection.Members[ungrantedID])
	}
	model, stopA := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" stop "+memberA.ID)
	_ = model
	if response := acceptanceTeamResponse(t, stopA, "team_member_stop"); response.TeamMember == nil {
		t.Fatalf("force stop did not return updated member: %+v", stopA.msgs)
	}
	waitTeamCapacityScenarioMemberStatus(t, project, sessionID, team.ID, memberA.ID, teams.MemberStopped)

	// Finish B's remaining accepted turns from the same member identity. Its
	// cumulative budget is visible as budget_exhausted and cannot be reset by
	// resume after sixteen accepted turns.
	for i := 1; i < teams.MaxMemberTurns; i++ {
		model, _ = submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" resume "+memberB.ID)
		got := receiveTeamCapacityScenarioInput(t, childRunner.started)
		if got.TeamTurn == nil || got.TeamTurn.MemberID != memberB.ID {
			t.Fatalf("budget turn %d ran for wrong member: %+v", i+1, got.TeamTurn)
		}
		childRunner.release <- struct{}{}
		waitTeamCapacityScenarioFinished(t, childRunner)
		wantStatus := teams.MemberIdle
		if i+1 == teams.MaxMemberTurns {
			wantStatus = teams.MemberBudgetExhausted
		}
		waitTeamCapacityScenarioMemberStatus(t, project, sessionID, team.ID, memberB.ID, wantStatus)
	}
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[memberB.ID]; got.Budget.AcceptedTurns != teams.MaxMemberTurns || got.Status != teams.MemberBudgetExhausted {
		t.Fatalf("cumulative budget state=%+v, want %d accepted turns and budget_exhausted", got, teams.MaxMemberTurns)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	model.Events = transcript.Events
	if err := model.showTeamMembers(team.ID, 0, ""); err != nil {
		t.Fatalf("render TUI member listing: %v", err)
	}
	var shown *teams.Member
	for i := range model.TeamMembers {
		if model.TeamMembers[i].ID == memberB.ID {
			member := model.TeamMembers[i]
			shown = &member
		}
	}
	if shown == nil || shown.Status != teams.MemberBudgetExhausted || shown.Budget.AcceptedTurns != teams.MaxMemberTurns {
		t.Fatalf("TUI member listing omitted cumulative budget status: %+v", model.TeamMembers)
	}
}

func submitTeamCapacityScenarioCommand(t *testing.T, model Model, text string) (Model, resultMsg) {
	t.Helper()
	model.Composer.SetValue(text)
	updated, cmd := model.submitComposer()
	got, ok := updated.(Model)
	if !ok || cmd == nil {
		t.Fatalf("submit %q: model=%T command=%v status=%q", text, updated, cmd != nil, model.Status)
	}
	response, ok := cmd().(resultMsg)
	if !ok {
		t.Fatalf("command %q returned non-result message", text)
	}
	if response.err != nil {
		got.Status = response.err.Error()
	}
	updated, _ = got.handleResult(response)
	got, ok = updated.(Model)
	if !ok {
		t.Fatalf("handle %q result returned non-Model", text)
	}
	return got, response
}

type teamCapacityScenarioRunner struct {
	started  chan agent.ChildRunInput
	release  chan struct{}
	finished chan struct{}
}

func (r *teamCapacityScenarioRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	select {
	case <-r.release:
		r.finished <- struct{}{}
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "capacity scenario turn complete"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func receiveTeamCapacityScenarioInput(t *testing.T, inputs <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-inputs:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shared-pool task")
		return agent.ChildRunInput{}
	}
}

func waitTeamCapacityScenarioFinished(t *testing.T, runner *teamCapacityScenarioRunner) {
	t.Helper()
	select {
	case <-runner.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child turn to finish")
	}
}

func waitTeamCapacityScenarioMemberStatus(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil {
			if member, ok := projection.Members[memberID]; ok && member.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("member %s status=%s, want %s; member=%+v", memberID, projection.Members[memberID].Status, want, projection.Members[memberID])
}
