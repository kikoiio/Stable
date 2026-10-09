package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// Exercise two logical turns through TUI commands and the real conversation
// socket. The fake runner gates each turn so the lead message is durably sent
// while the first turn is active, then explicitly resumed into the next turn.
func TestTeamTUITwoRoundResumeCarriesSummaryAndPendingMessage(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test fixture: %v", err)
		}
	})
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	childRunner := &twoRoundTeamChildRunner{started: make(chan agent.ChildRunInput, 2), release: make(chan struct{}, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
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
		select {
		case childRunner.release <- struct{}{}:
		default:
		}
		select {
		case childRunner.release <- struct{}{}:
		default:
		}
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
	parentRequest := agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "two-round team fixture", Messages: []llm.Message{{Role: "user", Content: "Start the team."}},
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if parentRun.request.RunID != parentRunID {
		t.Fatalf("active lead run=%q, want %q", parentRun.request.RunID, parentRunID)
	}
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for active lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create two-round")
	createdTeam := acceptanceTeamResponse(t, createResult, "team_create")
	if createdTeam.Team == nil {
		t.Fatalf("team create result=%+v", createdTeam)
	}
	teamID := createdTeam.Team.ID
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" spawn reader explore inspect the parser")
	spawned := acceptanceTeamResponse(t, spawnResult, "team_member_spawn")
	if spawned.TeamMember == nil {
		t.Fatalf("team spawn result=%+v", spawned)
	}
	memberID := spawned.TeamMember.ID
	first := receiveTwoRoundTeamChild(t, childRunner.started)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != memberID {
		t.Fatalf("first turn has wrong member identity: %+v", first.TeamTurn)
	}

	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" send "+memberID+" inspect parser recovery")
	sent := acceptanceTeamResponse(t, sendResult, "team_send")
	if sent.TeamMessage == nil || sent.TeamMessage.Body != "inspect parser recovery" {
		t.Fatalf("lead message was not persisted: %+v", sent)
	}
	childRunner.release <- struct{}{}
	waitTwoRoundTeamMemberStatus(t, project, sessionID, teamID, memberID, teams.MemberIdle)

	model, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" resume "+memberID)
	resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume")
	if resumed.TeamMember == nil || resumed.TeamMember.Status != teams.MemberQueued {
		t.Fatalf("resume result=%+v, want queued member", resumed)
	}
	second := receiveTwoRoundTeamChild(t, childRunner.started)
	if second.TeamTurn == nil || second.TeamTurn.MemberID != memberID || second.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("resume did not preserve member with a new turn: first=%+v second=%+v", first.TeamTurn, second.TeamTurn)
	}
	if !strings.Contains(second.Task.Instruction, "first-turn-summary") || !strings.Contains(second.Task.Instruction, "inspect parser recovery") {
		t.Fatalf("second turn omitted prior summary or pending lead message: %q", second.Task.Instruction)
	}
	childRunner.release <- struct{}{}
	waitTwoRoundTeamMemberStatus(t, project, sessionID, teamID, memberID, teams.MemberIdle)
}

type twoRoundTeamChildRunner struct {
	started chan agent.ChildRunInput
	release chan struct{}
}

func (r *twoRoundTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "first-turn-summary"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func receiveTwoRoundTeamChild(t *testing.T, started <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-started:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("conversation service did not start the next child turn")
		return agent.ChildRunInput{}
	}
}

func waitTwoRoundTeamMemberStatus(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
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
		t.Fatalf("replay team before status assertion: %v", err)
	}
	t.Fatalf("member status=%q, want %q", projection.Members[memberID].Status, want)
}
