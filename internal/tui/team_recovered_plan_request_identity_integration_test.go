package tui

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
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

// A pending plan request keeps its identity across a real service restart and
// socket reconnect. Recovery must not call the provider; responding to that
// same request is the explicit action that starts one read-only follow-up.
func TestTeamTUIApprovalUsesSameRecoveredPlanRequestAfterRestart(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tui-plan-request-restart-")
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

	roles := agentcatalog.New("", "")
	firstChild := &autoResumePlanChildRunner{stages: make(chan autoResumePlanStage, 1)}
	firstPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), firstChild, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstParent := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	firstSocket := filepath.Join(root, "first.sock")
	firstService, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: firstSocket, PollEvery: time.Hour,
		Runner: firstParent, Delegator: firstPool, Agents: roles,
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
		ToolSchemas:         []llm.ToolSchema{{Name: "read_file"}, {Name: "team_plan_submit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstChild.service = firstService
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			_ = firstService.Close()
		}
		firstPool.Close()
	})

	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(requestCtx, firstSocket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	firstLeadID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	firstStream, err := conversation.OpenRun(requestCtx, firstSocket, agent.ExecutionRequest{
		RunID: firstLeadID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "create pending plan request",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRun := receiveAcceptanceParentRun(t, firstParent.started)
	if started, err := firstStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("first lead run did not start: message=%+v err=%v", started, err)
	}
	model := New(firstSocket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, firstLeadID
	model, createTeamResult := submitAcceptanceTeamCommand(t, model, "/teams create recovered-plan")
	team := acceptanceTeamResponse(t, createTeamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect parser --plan")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil || !member.PlanRequired {
		t.Fatalf("spawn result=%+v; want plan-required member", member)
	}
	firstStage := receiveAutoResumePlanStage(t, firstChild.stages)
	if firstStage.err != nil || firstStage.input.TeamTurn == nil || firstStage.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial plan stage=%+v", firstStage)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberAwaitingPlan)
	initial, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pending teams.Request
	for _, candidate := range initial.Requests {
		if candidate.MemberID == member.ID && candidate.Type == teams.RequestPlan && candidate.Status == teams.RequestPending {
			pending = candidate
		}
	}
	if pending.ID == "" || pending.Revision != 1 {
		t.Fatalf("initial pending request=%+v", pending)
	}
	firstRun.finish(agent.RunCompleted)
	if err := firstStream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstService.Close(); err != nil {
		t.Fatal(err)
	}
	firstClosed = true
	firstPool.Close()

	secondChild := &recoveredPlanApprovalRunner{started: make(chan agent.ChildRunInput, 1)}
	secondPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), secondChild, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondParent := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	secondSocket := filepath.Join(root, "second.sock")
	secondService, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: secondSocket, PollEvery: time.Hour,
		Runner: secondParent, Delegator: secondPool, Agents: roles,
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
		ToolSchemas:         []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}, {Name: "command"}, {Name: "team_plan_submit"}},
	})
	if err != nil {
		t.Fatalf("restart conversation service: %v", err)
	}
	secondClosed := false
	t.Cleanup(func() {
		if !secondClosed {
			_ = secondService.Close()
		}
		secondPool.Close()
	})
	recovered, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Members[member.ID].Status != teams.MemberInterrupted || recovered.Requests[pending.ID].Status != teams.RequestPending || recovered.Requests[pending.ID].Revision != 1 {
		t.Fatalf("recovered member/request identity=%+v / %+v", recovered.Members[member.ID], recovered.Requests[pending.ID])
	}
	if secondChild.calls.Load() != 0 {
		t.Fatalf("service restart called provider %d times before explicit response", secondChild.calls.Load())
	}

	secondLeadID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secondStream, err := conversation.OpenRun(requestCtx, secondSocket, agent.ExecutionRequest{
		RunID: secondLeadID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "approve recovered plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRun := receiveAcceptanceParentRun(t, secondParent.started)
	if started, err := secondStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("reconnected lead run did not start: message=%+v err=%v", started, err)
	}
	model = New(secondSocket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, secondLeadID
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	var displayed teams.Request
	for _, candidate := range acceptanceTeamResponse(t, listResult, "team_request_list").TeamRequests {
		if candidate.ID == pending.ID {
			displayed = candidate
		}
	}
	if displayed.ID != pending.ID || displayed.Revision != pending.Revision || displayed.Status != teams.RequestPending {
		t.Fatalf("TUI reconnect displayed request=%+v; want original pending request %+v", displayed, pending)
	}
	_, responseResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+pending.ID+" 1 approve Continue read-only inspection.")
	approved := acceptanceTeamResponse(t, responseResult, "team_request_respond").TeamRequest
	if approved == nil || approved.ID != pending.ID || approved.Revision != 2 || approved.Status != teams.RequestApproved {
		t.Fatalf("approval did not resolve the same recovered request: %+v", approved)
	}
	if secondChild.calls.Load() != 0 {
		t.Fatalf("plan approval alone resumed a recovered interrupted member: calls=%d", secondChild.calls.Load())
	}
	_, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" resume "+member.ID)
	resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume").TeamMember
	if resumed == nil || (resumed.Status != teams.MemberQueued && resumed.Status != teams.MemberRunning) {
		t.Fatalf("explicit resume result=%+v", resumed)
	}
	followUp := receiveRecoveredPlanApproval(t, secondChild.started)
	if followUp.TeamTurn == nil || followUp.TeamTurn.MemberID != member.ID || followUp.ChildRunID == firstStage.input.ChildRunID || followUp.Task.ID == "" || followUp.Task.ID == firstStage.input.Task.ID {
		t.Fatalf("approved follow-up lost member/request identity: %+v", followUp)
	}
	allowed := map[string]bool{}
	for _, schema := range followUp.ToolSchemas {
		allowed[schema.Name] = true
	}
	if !allowed["read_file"] || allowed["write_file"] || allowed["command"] {
		t.Fatalf("recovered approved plan turn was not read-only: schemas=%v", followUp.ToolSchemas)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberIdle)
	final, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Requests[pending.ID].Revision != 2 || final.Requests[pending.ID].Status != teams.RequestApproved || final.Turns[followUp.TeamTurn.TurnID].PlanRequestID != pending.ID || secondChild.calls.Load() != 1 || len(final.Turns) != 2 {
		t.Fatalf("post-reconnect request/provider/turns=%+v/%d/%d", final.Requests[pending.ID], secondChild.calls.Load(), len(final.Turns))
	}
	secondRun.finish(agent.RunCompleted)
	if err := secondStream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secondService.Close(); err != nil {
		t.Fatal(err)
	}
	secondClosed = true
	secondPool.Close()
}

type recoveredPlanApprovalRunner struct {
	started chan agent.ChildRunInput
	calls   atomic.Int32
}

func (r *recoveredPlanApprovalRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.calls.Add(1)
	r.started <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "approved read-only follow-up completed"}
}

func receiveRecoveredPlanApproval(t *testing.T, started <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-started:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("recovered approved plan follow-up did not start")
		return agent.ChildRunInput{}
	}
}
