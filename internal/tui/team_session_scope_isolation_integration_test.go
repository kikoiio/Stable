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
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// Same-name teams and members remain owned by their session when requests pass
// through the real conversation socket and TUI command paths.
func TestTeamTUISessionScopeRejectsCrossSessionQueryMessageAndStop(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "tsi-")
	if err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() { _ = db.Close() })

	childRunner := &scopeIsolationChildRunner{started: make(chan agent.ChildRunInput, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 2, 2
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
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
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	createSession := func() string {
		t.Helper()
		messages, err := conversation.Request(requestCtx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
		if err != nil || len(messages) != 1 || messages[0].Session == nil {
			t.Fatalf("create session: messages=%+v err=%v", messages, err)
		}
		return messages[0].Session.ID
	}
	sessionA, sessionB := createSession(), createSession()
	openParent := func(sessionID string) (string, *conversation.StreamClient) {
		t.Helper()
		runID, err := sessionlog.NewID()
		if err != nil {
			t.Fatal(err)
		}
		stream, err := conversation.OpenRun(requestCtx, socket, agent.ExecutionRequest{
			RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "session scope isolation fixture",
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stream.Close() })
		parent := receiveAcceptanceParentRun(t, parentRunner.started)
		t.Cleanup(func() { parent.finish(agent.RunCompleted) })
		if parent.request.RunID != runID {
			t.Fatalf("parent run=%q, want %q", parent.request.RunID, runID)
		}
		if started, err := stream.Receive(); err != nil || started.Type != "run_started" || started.RunID != runID {
			t.Fatalf("start parent run: message=%+v err=%v", started, err)
		}
		return runID, stream
	}
	runA, _ := openParent(sessionA)
	runB, _ := openParent(sessionB)

	modelA, modelB := New(socket, project), New(socket, project)
	modelA.ActiveSession, modelA.ActiveRunID, modelA.Pending = sessionA, runA, true
	modelB.ActiveSession, modelB.ActiveRunID, modelB.Pending = sessionB, runB, true
	modelA, createA := submitAcceptanceTeamCommand(t, modelA, "/teams create research")
	teamA := acceptanceTeamResponse(t, createA, "team_create").Team
	modelB, createB := submitAcceptanceTeamCommand(t, modelB, "/teams create research")
	teamB := acceptanceTeamResponse(t, createB, "team_create").Team
	if teamA == nil || teamB == nil || teamA.ID == teamB.ID || teamA.Name != teamB.Name || teamA.Scope.SessionID == teamB.Scope.SessionID {
		t.Fatalf("same-name team identities/scopes not independent: A=%+v B=%+v", teamA, teamB)
	}
	modelA, spawnA := submitAcceptanceTeamCommand(t, modelA, "/team "+teamA.ID+" spawn reader explore inspect session A")
	memberA := acceptanceTeamResponse(t, spawnA, "team_member_spawn").TeamMember
	modelB, spawnB := submitAcceptanceTeamCommand(t, modelB, "/team "+teamB.ID+" spawn reader explore inspect session B")
	memberB := acceptanceTeamResponse(t, spawnB, "team_member_spawn").TeamMember
	if memberA == nil || memberB == nil || memberA.ID == memberB.ID || memberA.Name != memberB.Name {
		t.Fatalf("same-name member identities not independent: A=%+v B=%+v", memberA, memberB)
	}
	for range 2 {
		select {
		case <-childRunner.started:
		case <-requestCtx.Done():
			t.Fatal("session member child did not start")
		}
	}
	waitSessionScopeMemberIdle(t, project, sessionA, teamA.ID, memberA.ID)
	waitSessionScopeMemberIdle(t, project, sessionB, teamB.ID, memberB.ID)

	modelB, listB := submitAcceptanceTeamCommand(t, modelB, "/teams list")
	listedB := acceptanceTeamResponse(t, listB, "team_list").Teams
	if len(listedB) != 1 || listedB[0].ID != teamB.ID {
		t.Fatalf("session B team list leaked another session: %+v", listedB)
	}
	modelB, gotB := submitAcceptanceTeamCommand(t, modelB, "/team "+teamB.ID+" get")
	if own := acceptanceTeamResponse(t, gotB, "team_get").Team; own == nil || own.ID != teamB.ID {
		t.Fatalf("session B could not query its own team: %+v", own)
	}
	expectTeamCommandError(t, modelB, "/team "+teamA.ID+" get")
	expectTeamCommandError(t, modelB, "/team "+teamA.ID+" send "+memberA.ID+" cross-session delivery")
	expectTeamCommandError(t, modelB, "/team "+teamA.ID+" stop "+memberA.ID)

	modelA, messagesA := submitAcceptanceTeamCommand(t, modelA, "/team "+teamA.ID+" messages")
	if got := acceptanceTeamResponse(t, messagesA, "team_messages").TeamMessages; len(got) != 0 {
		t.Fatalf("rejected cross-session message changed session A delivery log: %+v", got)
	}
	projectionA, err := sessionlog.ReplayTeams(project, sessionA, teamA.ID)
	if err != nil {
		t.Fatal(err)
	}
	projectionB, err := sessionlog.ReplayTeams(project, sessionB, teamB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projectionA.Members[memberA.ID].Status != teams.MemberIdle || projectionB.Members[memberB.ID].Status != teams.MemberIdle || len(projectionA.Messages) != 0 || len(projectionB.Messages) != 0 {
		t.Fatalf("cross-session commands mutated team facts: A=%+v B=%+v", projectionA, projectionB)
	}
}

type scopeIsolationChildRunner struct{ started chan agent.ChildRunInput }

func (r *scopeIsolationChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "scope isolated"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func expectTeamCommandError(t *testing.T, model Model, command string) {
	t.Helper()
	model.Composer.SetValue(command)
	updated, cmd := model.submitComposer()
	got, ok := updated.(Model)
	if !ok || cmd == nil {
		t.Fatalf("submit cross-session command %q: model=%T command=%v", command, updated, cmd != nil)
	}
	result, ok := cmd().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("cross-session command %q result=%+v, want scope rejection", command, result)
	}
	updated, _ = got.handleResult(result)
	if _, ok := updated.(Model); !ok {
		t.Fatalf("handle cross-session command returned %T", updated)
	}
}

func waitSessionScopeMemberIdle(t *testing.T, root, sessionID, teamID, memberID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == teams.MemberIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatalf("replay team after child completion: %v", err)
	}
	t.Fatalf("session %s member status=%q, want idle", sessionID, projection.Members[memberID].Status)
}
