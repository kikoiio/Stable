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

// This crosses the slash command, Unix socket, conversation service and replay
// boundary for a lead response to a real pending child plan request.
func TestTeamPlanRequestTUIResponsePersistsLeadApproval(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "m09-pr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project := filepath.Join(root, "p")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	childRunner := &planSubmittingChildRunner{started: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "plan approval fixture",
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
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create plan-review")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect parser --plan")
	spawned := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if spawned == nil || !spawned.PlanRequired {
		t.Fatalf("spawn response=%+v, want plan-required member", spawned)
	}
	select {
	case <-childRunner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not submit plan request")
	}
	model.ActiveRunID = "" // local user approval is derived from the trusted team scope.
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	listed := acceptanceTeamResponse(t, listResult, "team_request_list").TeamRequests
	var pending teams.Request
	for _, request := range listed {
		if request.Type == teams.RequestPlan && request.MemberID == spawned.ID && request.Status == teams.RequestPending {
			pending = request
		}
	}
	if pending.ID == "" || pending.Revision != 1 || pending.RequesterID != spawned.ID || pending.ResponderID != teams.Lead {
		t.Fatalf("pending plan request=%+v, want member requester, lead responder, revision 1", pending)
	}

	_, responseResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+pending.ID+" 1 approve Proceed with the read-only review.")
	response := acceptanceTeamResponse(t, responseResult, "team_request_respond").TeamRequest
	if response == nil || response.Status != teams.RequestApproved || response.Revision != 2 || response.Feedback != "Proceed with the read-only review." {
		t.Fatalf("TUI response=%+v, want approved revision 2 with feedback", response)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed := projection.Requests[pending.ID]
	member := projection.Members[spawned.ID]
	if replayed.Status != teams.RequestApproved || replayed.Revision != 2 || replayed.ResponderID != teams.Lead || replayed.Feedback != response.Feedback || !member.PlanApproved {
		t.Fatalf("replayed request=%+v member=%+v, want durable lead approval", replayed, member)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var leadResponseEvents int
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		data, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		var fact sessionlog.TeamEvent
		if err := json.Unmarshal(data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamRequestResponded && fact.Request != nil && fact.Request.ID == pending.ID {
			if fact.ActorID != teams.Lead || fact.Request.Status != teams.RequestApproved || fact.Request.Revision != 2 {
				t.Fatalf("durable response actor/request = %q / %+v, want lead revision-2 approval", fact.ActorID, fact.Request)
			}
			leadResponseEvents++
		}
	}
	if leadResponseEvents != 1 {
		t.Fatalf("durable lead response events=%d, want exactly one", leadResponseEvents)
	}
}

type planSubmittingChildRunner struct {
	service *conversation.Service
	started chan struct{}
}

func (r *planSubmittingChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	if input.TeamTurn == nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "missing trusted team turn"}
	}
	_, err := r.service.ExecuteTeamTool(ctx, agent.ExecutionRequest{
		RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn,
	}, llm.ToolUse{
		ID: "submit-plan", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + input.TeamTurn.TeamID + `","body":"Inspect parser entry point and report findings."}`),
	})
	if err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
	}
	r.started <- struct{}{}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "submitted plan"}
}
