package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// Canceling one lead run from the TUI socket cancels only the team child that
// was spawned by that run; another parent run's child and stream continue.
func TestTeamTUIParentCancelOnlyInterruptsItsOwnMemberTurn(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "tpc-")
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

	childRunner := &parentCancelScopeChildRunner{started: make(chan parentCancelScopeChildStart, 2), release: make(chan string, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 2, 2
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 2 {
			select {
			case childRunner.release <- "cleanup":
			default:
			}
		}
		pool.Close()
	})
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 2)}
	socket := filepath.Join(root, "s")
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

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	openParent := func() (string, *conversation.StreamClient, *acceptanceTeamParentRun) {
		t.Helper()
		runID, err := sessionlog.NewID()
		if err != nil {
			t.Fatal(err)
		}
		stream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
			RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "scoped team cancellation fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stream.Close() })
		parent := receiveAcceptanceParentRun(t, parentRunner.started)
		t.Cleanup(func() { parent.finish(agent.RunCompleted) })
		if started, err := stream.Receive(); err != nil || started.Type != "run_started" || started.RunID != runID {
			t.Fatalf("start parent run: message=%+v err=%v", started, err)
		}
		return runID, stream, parent
	}
	runA, streamA, parentA := openParent()
	runB, streamB, parentB := openParent()

	modelA, modelB := New(socket, project), New(socket, project)
	modelA.ActiveSession, modelA.ActiveRunID, modelA.Pending, modelA.stream = sessionID, runA, true, streamA
	modelB.ActiveSession, modelB.ActiveRunID, modelB.Pending, modelB.stream = sessionID, runB, true, streamB
	modelA, createResult := submitAcceptanceTeamCommand(t, modelA, "/teams create cancel-scope")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("team create omitted team")
	}
	modelA, spawnAResult := submitAcceptanceTeamCommand(t, modelA, "/team "+team.ID+" spawn reader-a explore inspect area A")
	memberA := acceptanceTeamResponse(t, spawnAResult, "team_member_spawn").TeamMember
	modelB, spawnBResult := submitAcceptanceTeamCommand(t, modelB, "/team "+team.ID+" spawn reader-b explore inspect area B")
	memberB := acceptanceTeamResponse(t, spawnBResult, "team_member_spawn").TeamMember
	if memberA == nil || memberB == nil || memberA.ID == memberB.ID {
		t.Fatalf("spawned members=%+v/%+v", memberA, memberB)
	}
	children := make(map[string]parentCancelScopeChildStart, 2)
	for range 2 {
		select {
		case child := <-childRunner.started:
			if child.input.TeamTurn == nil {
				t.Fatalf("started child lacks team turn: %+v", child.input)
			}
			children[child.input.TeamTurn.MemberID] = child
		case <-reqctx.Done():
			t.Fatal("both team children did not reach their gates")
		}
	}
	childA, okA := children[memberA.ID]
	childB, okB := children[memberB.ID]
	if !okA || !okB || childA.input.ParentRunID != runA || childB.input.ParentRunID != runB {
		t.Fatalf("children not attributed to their parent runs: A=%+v B=%+v", childA.input, childB.input)
	}

	updated, cmd := modelA.Update(tea.KeyMsg{Type: tea.KeyEsc})
	modelA, ok := updated.(Model)
	if !ok || cmd == nil {
		t.Fatalf("TUI escape produced model=%T cancel command=%v", updated, cmd != nil)
	}
	cancelResult, ok := cmd().(runStreamMsg)
	if !ok || cancelResult.err != nil || cancelResult.message.Type != "cancel_sent" {
		t.Fatalf("TUI parent cancel result=%T %+v", cancelResult, cancelResult)
	}
	select {
	case <-childA.ctx.Done():
	case <-reqctx.Done():
		t.Fatal("canceling parent A did not cancel member A")
	}
	select {
	case <-childB.ctx.Done():
		t.Fatal("canceling parent A also canceled parent B's member")
	default:
	}
	waitParentCancelScopeMember(t, project, sessionID, team.ID, memberA.ID, teams.MemberInterrupted)
	waitParentCancelScopeMember(t, project, sessionID, team.ID, memberB.ID, teams.MemberRunning)

	parentB.events <- agent.ExecutionEvent{
		ID: "parent-b-after-a-cancel", RunID: runB, SessionID: sessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: agent.EventTextDelta,
	}
	streamMessage, err := streamB.Receive()
	if err != nil || streamMessage.Type != "run_event" || streamMessage.RunEvent == nil || streamMessage.RunEvent.ID != "parent-b-after-a-cancel" {
		t.Fatalf("parent B stream stopped after canceling A: message=%+v err=%v", streamMessage, err)
	}
	childRunner.release <- memberB.ID
	waitParentCancelScopeMember(t, project, sessionID, team.ID, memberB.ID, teams.MemberIdle)
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Turns[childA.input.TeamTurn.TurnID].Status != string(agent.DelegationCanceled) || projection.Turns[childB.input.TeamTurn.TurnID].Status != string(agent.DelegationSucceeded) {
		t.Fatalf("parent-scoped terminal turns A=%+v B=%+v", projection.Turns[childA.input.TeamTurn.TurnID], projection.Turns[childB.input.TeamTurn.TurnID])
	}
	_ = parentA // parent A is intentionally left canceled while the independent parent B completed its child.
}

type parentCancelScopeChildStart struct {
	input agent.ChildRunInput
	ctx   context.Context
}

type parentCancelScopeChildRunner struct {
	started chan parentCancelScopeChildStart
	release chan string
}

func (r *parentCancelScopeChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- parentCancelScopeChildStart{input: input, ctx: ctx}:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	}
	select {
	case memberID := <-r.release:
		if input.TeamTurn != nil && memberID == input.TeamTurn.MemberID {
			return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "child completed independently"}
		}
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "released wrong member"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	}
}

func waitParentCancelScopeMember(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatalf("replay team after parent cancellation: %v", err)
	}
	t.Fatalf("member %s status=%q, want %q", memberID, projection.Members[memberID].Status, want)
}
