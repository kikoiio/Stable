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
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// A conflicting response through the user-facing TUI path must leave an
// already approved plan and its member state unchanged in the durable log.
func TestTeamPlanTUIConflictingTerminalResponseIsNoOp(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tui-terminal-")
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

	childRunner := &planSubmittingChildRunner{started: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
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
	childRunner.service = svc
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "terminal response fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create terminal-response")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect parser --plan")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil || !member.PlanRequired {
		t.Fatalf("spawn response=%+v, want plan-required member", member)
	}
	select {
	case <-childRunner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not submit plan request")
	}
	model.ActiveRunID = ""
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	listed := acceptanceTeamResponse(t, listResult, "team_request_list").TeamRequests
	var pending teams.Request
	for _, request := range listed {
		if request.Type == teams.RequestPlan && request.MemberID == member.ID && request.Status == teams.RequestPending {
			pending = request
		}
	}
	if pending.ID == "" || pending.Revision != 1 {
		t.Fatalf("pending plan request=%+v, want revision 1", pending)
	}

	_, approveResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+pending.ID+" 1 approve Allow the scoped read-only follow-up.")
	approved := acceptanceTeamResponse(t, approveResult, "team_request_respond").TeamRequest
	if approved == nil || approved.Status != teams.RequestApproved || approved.Revision != 2 {
		t.Fatalf("approval response=%+v, want approved revision 2", approved)
	}
	model.Composer.SetValue("/team " + team.ID + " respond " + pending.ID + " 2 reject conflicting second decision")
	_, cmd := model.submitComposer()
	if cmd == nil {
		t.Fatal("conflicting terminal response produced no TUI command")
	}
	result, ok := cmd().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("conflicting terminal response result=%T %+v, want reported error", result, result)
	}

	after, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Approval automatically starts a follow-up member turn, so unrelated
	// child run facts may legitimately arrive while the rejected response is
	// being checked. The request-specific response count below is the durable
	// no-op invariant for this conflicting decision.
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[pending.ID]; got.Status != teams.RequestApproved || got.Revision != 2 || got.Feedback != approved.Feedback {
		t.Fatalf("conflicting response changed approved request: %+v", got)
	}
	if got := projection.Members[member.ID]; !got.PlanApproved {
		t.Fatalf("conflicting response revoked plan approval: %+v", got)
	}
	responses := 0
	for _, event := range after.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		raw, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		var fact sessionlog.TeamEvent
		if err := json.Unmarshal(raw, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamRequestResponded && fact.Request != nil && fact.Request.ID == pending.ID {
			responses++
			if fact.Request.Status != teams.RequestApproved || fact.Request.Revision != 2 {
				t.Fatalf("unexpected durable response fact: %+v", fact)
			}
		}
	}
	if responses != 1 {
		t.Fatalf("durable response facts=%d, want exactly one", responses)
	}
}
