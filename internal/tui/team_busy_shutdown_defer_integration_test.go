package tui

import (
	"context"
	"encoding/json"
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

// A lead's busy-member shutdown request crosses TUI/socket, is deferred by the
// member's trusted tool identity, and leaves its in-flight turn running.
func TestTeamTUIBusyShutdownCanBeDeferredWhileMemberContinues(t *testing.T) {
	ctx := context.Background()
	tmpRoot := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpRoot, "tbsd-")
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

	childRunner := &busyShutdownDeferRunner{started: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case childRunner.release <- struct{}{}:
		default:
		}
		pool.Close()
	})
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "busy shutdown deferral fixture",
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
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create busy-shutdown-defer")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("team create omitted team")
	}
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("member spawn omitted member")
	}
	var child agent.ChildRunInput
	select {
	case child = <-childRunner.started:
	case <-reqctx.Done():
		t.Fatal("member child did not reach its running gate")
	}
	if child.TeamTurn == nil || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("busy child turn identity=%+v", child.TeamTurn)
	}

	model, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+member.ID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestPending || shutdown.Revision != 1 {
		t.Fatalf("busy shutdown request=%+v, want pending revision 1", shutdown)
	}
	arguments, err := json.Marshal(map[string]any{
		"team_id": team.ID, "request_id": shutdown.ID, "expected_revision": shutdown.Revision,
		"decision": string(teams.RequestDeferred), "feedback": "Finish the current inspection first.",
	})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := svc.ExecuteTeamTool(reqctx, agent.ExecutionRequest{
		RunID: child.ChildRunID, Work: child.Work, TeamTurn: child.TeamTurn,
	}, llm.ToolUse{ID: "defer-busy-shutdown", Name: "team_request_respond", Arguments: arguments})
	if err != nil || deferred.Status != agent.ToolSucceeded || deferred.IsError {
		t.Fatalf("member defer tool outcome=%+v err=%v", deferred, err)
	}

	model, requestsResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	requests := acceptanceTeamResponse(t, requestsResult, "team_request_list").TeamRequests
	var deferredVisible bool
	for _, request := range requests {
		if request.ID == shutdown.ID && request.Status == teams.RequestDeferred && request.Revision == 2 && request.Feedback == "Finish the current inspection first." {
			deferredVisible = true
		}
	}
	if !deferredVisible {
		t.Fatalf("TUI request list omitted the member's deferral: %+v", requests)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[member.ID].Status != teams.MemberRunning || projection.Requests[shutdown.ID].Status != teams.RequestDeferred {
		t.Fatalf("deferred shutdown changed active member: member=%+v request=%+v", projection.Members[member.ID], projection.Requests[shutdown.ID])
	}
	select {
	case <-childRunner.release:
	default:
		childRunner.release <- struct{}{}
	}
	waitTwoRoundTeamMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Requests[shutdown.ID].Status != teams.RequestDeferred || len(projection.Turns) != 1 {
		t.Fatalf("deferred request or active turn changed after child completion: request=%+v turns=%+v", projection.Requests[shutdown.ID], projection.Turns)
	}
}

type busyShutdownDeferRunner struct {
	started chan agent.ChildRunInput
	release chan struct{}
}

func (r *busyShutdownDeferRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "current inspection finished"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}
