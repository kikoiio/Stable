package tui

import (
	"context"
	"os"
	"path/filepath"
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

// This test deliberately crosses the same slash-command, Unix-socket and
// conversation service boundaries used by the interactive TUI. The parent
// runner only holds an active lead run; the bounded-pool child runner is fake
// and never calls the provider or executes tools.
func TestTeamTUIAcceptanceCreateSpawnSendListAndGetOverConversationSocket(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
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

	childRunner := &acceptanceTeamChildRunner{started: make(chan agent.ChildRunInput, 2), release: make(chan struct{}, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socketDir, err := os.MkdirTemp(os.TempDir(), "m09-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "conversation.sock")
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
	t.Cleanup(func() {
		select {
		case childRunner.release <- struct{}{}:
		default:
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
		Intent: "acceptance fixture lead", Messages: []llm.Message{{Role: "user", Content: "Start team acceptance."}},
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
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for active lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, createdTeam := submitAcceptanceTeamCommand(t, model, "/teams create acceptance")
	createdResponse := acceptanceTeamResponse(t, createdTeam, "team_create")
	if createdResponse.Team == nil || createdResponse.Team.Name != "acceptance" {
		t.Fatalf("TUI create result=%+v, want service-created acceptance team", createdResponse)
	}
	teamID := createdResponse.Team.ID

	model, spawned := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" spawn reader explore inspect the parser")
	spawnResponse := acceptanceTeamResponse(t, spawned, "team_member_spawn")
	if spawnResponse.TeamMember == nil || spawnResponse.TeamMember.Name != "reader" {
		t.Fatalf("TUI spawn result=%+v, want service-created reader member", spawnResponse)
	}
	memberID := spawnResponse.TeamMember.ID
	select {
	case child := <-childRunner.started:
		if child.TeamTurn == nil || child.TeamTurn.TeamID != teamID || child.TeamTurn.MemberID != memberID {
			t.Fatalf("fake child runner received unscoped team turn: %+v", child)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conversation service did not start the spawned child through the fake runner")
	}

	model, sent := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" send "+memberID+" inspect parser recovery")
	sendResponse := acceptanceTeamResponse(t, sent, "team_send")
	if sendResponse.TeamMessage == nil || sendResponse.TeamMessage.Body != "inspect parser recovery" || sendResponse.TeamMessage.TeamID != teamID {
		t.Fatalf("TUI send result=%+v, want persisted team message", sendResponse)
	}
	model, listed := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" messages")
	listResponse := acceptanceTeamResponse(t, listed, "team_messages")
	if len(listResponse.TeamMessages) != 1 || listResponse.TeamMessages[0].Body != "inspect parser recovery" {
		t.Fatalf("TUI messages result=%+v, want one service-persisted message", listResponse)
	}
	model, got := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" get")
	getResponse := acceptanceTeamResponse(t, got, "team_get")
	if getResponse.Team == nil || getResponse.Team.ID != teamID || getResponse.Team.Name != "acceptance" {
		t.Fatalf("TUI get result=%+v, want created team", getResponse)
	}
	childRunner.release <- struct{}{}
	waitAcceptanceTeamMemberIdle(t, project, sessionID, teamID, memberID)
}

func waitAcceptanceTeamMemberIdle(t *testing.T, project, sessionID, teamID, memberID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
		if err == nil {
			if member, ok := projection.Members[memberID]; ok && member.Status == teams.MemberIdle {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("team member status=%s, want idle before closing the fixture", projection.Members[memberID].Status)
}

func submitAcceptanceTeamCommand(t *testing.T, model Model, text string) (Model, resultMsg) {
	t.Helper()
	model.Composer.SetValue(text)
	updated, cmd := model.submitComposer()
	got, ok := updated.(Model)
	if !ok || cmd == nil {
		t.Fatalf("submit %q: model=%T command=%v status=%q", text, updated, cmd != nil, model.Status)
	}
	response := cmd()
	result, ok := response.(resultMsg)
	if !ok {
		t.Fatalf("command %q returned %T, want resultMsg", text, response)
	}
	if result.err != nil {
		t.Fatalf("command %q failed over conversation socket: %v", text, result.err)
	}
	updated, _ = got.handleResult(result)
	got, ok = updated.(Model)
	if !ok {
		t.Fatalf("handle %q result returned %T, want Model", text, updated)
	}
	return got, result
}

func acceptanceTeamResponse(t *testing.T, result resultMsg, typ string) conversation.ServerMsg {
	t.Helper()
	for _, msg := range result.msgs {
		if msg.Type == typ {
			return msg
		}
	}
	t.Fatalf("response %q missing from %+v", typ, result.msgs)
	return conversation.ServerMsg{}
}

type acceptanceTeamParentRun struct {
	request agent.ExecutionRequest
	events  chan agent.ExecutionEvent
	done    chan agent.RunOutcome
}

func (r *acceptanceTeamParentRun) finish(status agent.RunStatus) {
	select {
	case <-r.done:
		return
	default:
	}
	close(r.events)
	r.done <- agent.RunOutcome{RunID: r.request.RunID, Status: status}
	close(r.done)
}

type acceptanceTeamParentRunner struct{ started chan *acceptanceTeamParentRun }

func (r *acceptanceTeamParentRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	run := &acceptanceTeamParentRun{request: request, events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1)}
	r.started <- run
	return &agent.RunHandle{Events: run.events, Done: run.done}, nil
}

func (*acceptanceTeamParentRunner) Cancel(string) error { return nil }

func receiveAcceptanceParentRun(t *testing.T, started <-chan *acceptanceTeamParentRun) *acceptanceTeamParentRun {
	t.Helper()
	select {
	case run := <-started:
		return run
	case <-time.After(5 * time.Second):
		t.Fatal("parent runner did not start")
		return nil
	}
}

type acceptanceTeamChildRunner struct {
	started chan agent.ChildRunInput
	release chan struct{}
}

func (r *acceptanceTeamChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	if r.release == nil {
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "fake child completed"}
	}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "fake child completed"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

type acceptanceTeamProvider struct{}

func (acceptanceTeamProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	events, errs := make(chan llm.Event), make(chan error)
	close(events)
	close(errs)
	return events, errs
}
