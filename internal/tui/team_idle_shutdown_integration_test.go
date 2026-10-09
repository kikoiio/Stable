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

// An idle member's graceful shutdown must cross the real TUI/socket/service
// path, persist a typed approval, and stop the member without another turn.
func TestTeamTUIIdleShutdownPersistsApprovalWithoutAnotherTurn(t *testing.T) {
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

	childRunner := &acceptanceTeamChildRunner{started: make(chan agent.ChildRunInput, 2)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socketDir, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "tis-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketDir, err = filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "s")
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
	parentStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "idle shutdown fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create idle-shutdown")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI spawn response omitted member")
	}
	select {
	case child := <-childRunner.started:
		if child.TeamTurn == nil || child.TeamTurn.TeamID != team.ID || child.TeamTurn.MemberID != member.ID {
			t.Fatalf("fake child received wrong team turn: %+v", child.TeamTurn)
		}
	case <-reqctx.Done():
		t.Fatal("conversation service did not start the member child")
	}
	waitIdleShutdownMemberStatus(t, project, sessionID, team.ID, member.ID)

	_, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+member.ID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.ID == "" || shutdown.TeamID != team.ID || shutdown.MemberID != member.ID || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestApproved || shutdown.Revision != 2 || shutdown.RequesterID != teams.Lead || shutdown.ResponderID != member.ID {
		t.Fatalf("TUI idle shutdown request=%+v, want approved typed request at revision 2", shutdown)
	}

	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberStopped {
		t.Fatalf("replayed member status=%s, want stopped", got)
	}
	if got := projection.Requests[shutdown.ID]; got != *shutdown {
		t.Fatalf("replayed shutdown request=%+v, want TUI response %+v", got, *shutdown)
	}
	if len(projection.Turns) != 1 {
		t.Fatalf("member turns after idle shutdown=%d, want only the initial turn", len(projection.Turns))
	}
	select {
	case extra := <-childRunner.started:
		t.Fatalf("idle shutdown started another child turn: %+v", extra.TeamTurn)
	default:
	}

	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	createdFacts, respondedFacts, turnIntents, turnAcceptances := 0, 0, 0, 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeTUIEvent(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.TeamID != team.ID {
			continue
		}
		switch fact.Kind {
		case sessionlog.TeamRequestCreated:
			if fact.Request != nil && fact.Request.ID == shutdown.ID {
				createdFacts++
				if fact.ActorID != teams.Lead || fact.Request.Type != teams.RequestShutdown || fact.Request.Status != teams.RequestPending {
					t.Fatalf("durable shutdown creation fact=%+v", fact)
				}
			}
		case sessionlog.TeamRequestResponded:
			if fact.Request != nil && fact.Request.ID == shutdown.ID {
				respondedFacts++
				if fact.ActorID != "service" || fact.Request.Status != teams.RequestApproved || fact.Request.Revision != 2 {
					t.Fatalf("durable shutdown response fact=%+v", fact)
				}
			}
		case sessionlog.TeamTurnIntent:
			turnIntents++
		case sessionlog.TeamTurnAccepted:
			turnAcceptances++
		}
	}
	if createdFacts != 1 || respondedFacts != 1 {
		t.Fatalf("shutdown durable facts: created=%d responded=%d, want one each", createdFacts, respondedFacts)
	}
	if turnIntents != 1 || turnAcceptances != 1 {
		t.Fatalf("member turn facts after shutdown: intents=%d acceptances=%d, want one initial turn only", turnIntents, turnAcceptances)
	}
}

func waitIdleShutdownMemberStatus(t *testing.T, root, sessionID, teamID, memberID string) {
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
		t.Fatal(err)
	}
	t.Fatalf("member status=%s, want idle before graceful shutdown", projection.Members[memberID].Status)
}
