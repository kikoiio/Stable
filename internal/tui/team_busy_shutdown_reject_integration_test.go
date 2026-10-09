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

// A busy member can reject a typed shutdown request through its trusted turn
// identity while the active child continues to its normal completion.
func TestTeamTUIBusyShutdownCanBeRejectedWhileMemberContinues(t *testing.T) {
	ctx := context.Background()
	tmpRoot := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpRoot, "tbsr-")
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "busy shutdown rejection fixture",
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
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create busy-shutdown-reject")
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
	if child.TeamTurn == nil || child.TeamTurn.TeamID != team.ID || child.TeamTurn.MemberID != member.ID {
		t.Fatalf("busy child turn identity=%+v", child.TeamTurn)
	}

	model, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+member.ID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestPending || shutdown.Revision != 1 {
		t.Fatalf("busy shutdown request=%+v, want pending revision 1", shutdown)
	}
	feedback := "Continue the current inspection before stopping."
	arguments, err := json.Marshal(map[string]any{
		"team_id": team.ID, "request_id": shutdown.ID, "expected_revision": shutdown.Revision,
		"decision": string(teams.RequestRejected), "feedback": feedback,
	})
	if err != nil {
		t.Fatal(err)
	}
	memberRequest := agent.ExecutionRequest{RunID: child.ChildRunID, Work: child.Work, TeamTurn: child.TeamTurn}
	rejected, err := svc.ExecuteTeamTool(reqctx, memberRequest, llm.ToolUse{
		ID: "reject-busy-shutdown", Name: "team_request_respond", Arguments: arguments,
	})
	if err != nil || rejected.Status != agent.ToolSucceeded || rejected.IsError {
		t.Fatalf("member reject tool outcome=%+v err=%v", rejected, err)
	}

	model, requestsResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	requests := acceptanceTeamResponse(t, requestsResult, "team_request_list").TeamRequests
	var rejectedVisible bool
	for _, request := range requests {
		if request.ID == shutdown.ID && request.Status == teams.RequestRejected && request.Revision == 2 && request.Feedback == feedback && request.ResponderID == member.ID {
			rejectedVisible = true
		}
	}
	if !rejectedVisible {
		t.Fatalf("TUI request list omitted the member's rejection: %+v", requests)
	}

	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn := projection.Turns[child.TeamTurn.TurnID]
	if projection.Members[member.ID].Status != teams.MemberRunning || turn.Status == string(agent.DelegationSucceeded) || turn.Status == string(agent.DelegationFailed) || turn.Status == string(agent.DelegationCanceled) || turn.Status == string(agent.DelegationInterrupted) || projection.Requests[shutdown.ID].Status != teams.RequestRejected {
		t.Fatalf("rejected shutdown interrupted active child: member=%+v turn=%+v request=%+v", projection.Members[member.ID], turn, projection.Requests[shutdown.ID])
	}

	childRunner.release <- struct{}{}
	waitTwoRoundTeamMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Requests[shutdown.ID].Status != teams.RequestRejected || projection.Members[member.ID].Status != teams.MemberIdle || projection.Turns[child.TeamTurn.TurnID].Status != string(agent.DelegationSucceeded) {
		t.Fatalf("rejected request changed after natural child completion: request=%+v member=%+v turn=%+v", projection.Requests[shutdown.ID], projection.Members[member.ID], projection.Turns[child.TeamTurn.TurnID])
	}
}
