package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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

// TestTeamPlanTUIRejectReviseApproveAndAutoReadOnlyFollowUp crosses the TUI socket,
// conversation service, delegation runner and durable team projection. A lead
// rejects the first plan, the member revises it, then approval starts a new
// read-only follow-up turn.
func TestTeamPlanTUIRejectReviseApproveAndAutoReadOnlyFollowUp(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tui-plan-revise-")
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

	readExecutor := &agent.FakeExecutor{Script: []agent.ToolOutcome{{Content: "parser entry point is parseDocument"}}}
	childRunner := &planRevisionChildRunner{stages: make(chan planRevisionStage, 3)}
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
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: readExecutor},
		ToolSchemas: []llm.ToolSchema{
			{Name: "read_file"}, {Name: "write_file"}, {Name: "edit_file"}, {Name: "command"},
			{Name: "team_plan_submit"}, {Name: "team_request_list"}, {Name: "team_send"},
		},
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

	reqctx, cancel := context.WithTimeout(ctx, 8*time.Second)
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "plan rejection/revision fixture",
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
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create plan-revision")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect parser --plan")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil || !member.PlanRequired {
		t.Fatalf("spawn response=%+v, want plan-required member", member)
	}
	first := receivePlanRevisionStage(t, childRunner.stages)
	if first.index != 1 || first.err != nil || first.input.TeamTurn == nil || first.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("first child stage=%+v; want successful plan submission for member", first)
	}
	model.ActiveRunID = "" // lead approval requests are bound to the durable session/team scope.
	pending := pendingPlanFromTUI(t, model, team.ID, member.ID)
	if pending.Revision != 1 || pending.RequesterID != member.ID || pending.ResponderID != teams.Lead || !strings.Contains(pending.Body, "Initial plan") {
		t.Fatalf("initial pending plan=%+v", pending)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberAwaitingPlan)
	assertRejectedTeamResponseDoesNotMutate(t, model, project, sessionID, team.ID, "unknown-plan-request", pending.Revision, "approve")
	assertRejectedTeamResponseDoesNotMutate(t, model, project, sessionID, team.ID, pending.ID, pending.Revision+1, "approve")

	_, rejectResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+pending.ID+" 1 reject Include the parser entry point and keep the follow-up read-only.")
	rejected := acceptanceTeamResponse(t, rejectResult, "team_request_respond").TeamRequest
	if rejected == nil || rejected.Status != teams.RequestRejected || rejected.Revision != 2 {
		t.Fatalf("lead rejection=%+v, want rejected revision 2", rejected)
	}
	// Rejection itself should wake the paused member. Do not issue /resume:
	// this verifies continuation from the persisted decision alone.
	second := receivePlanRevisionStageFor(t, childRunner.stages, project, sessionID, team.ID, member.ID)
	if second.index != 2 || second.err != nil || second.input.TeamTurn == nil || second.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("revision child stage=%+v; want member to revise plan", second)
	}
	if !strings.Contains(second.input.Task.Instruction, "Include the parser entry point") {
		t.Fatalf("rejection feedback missing from revised turn instruction: %q", second.input.Task.Instruction)
	}
	revised := pendingPlanFromTUI(t, model, team.ID, member.ID)
	if revised.ID == pending.ID || revised.Revision != 1 || !strings.Contains(revised.Body, "Revised plan") || revised.Status != teams.RequestPending {
		t.Fatalf("revised pending plan=%+v, initial=%+v", revised, pending)
	}
	model.ActiveRunID = "" // The durable lead/team scope authorizes the response.
	_, approveResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+revised.ID+" 1 approve Proceed with the read-only follow-up.")
	approved := acceptanceTeamResponse(t, approveResult, "team_request_respond").TeamRequest
	if approved == nil || approved.Status != teams.RequestApproved || approved.Revision != 2 {
		t.Fatalf("lead approval=%+v, want approved revision 2", approved)
	}
	// Approval likewise starts exactly one read-only follow-up without /resume.
	third := receivePlanRevisionStage(t, childRunner.stages)
	if third.index != 3 || third.err != nil || third.input.TeamTurn == nil || third.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("approved follow-up child stage=%+v", third)
	}
	if third.input.ChildRunID == second.input.ChildRunID || third.input.TeamTurn.TurnID == second.input.TeamTurn.TurnID {
		t.Fatalf("approved follow-up did not use fresh run/turn identity: revised=%+v follow-up=%+v", second.input, third.input)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberIdle)

	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[pending.ID]; got.Status != teams.RequestRejected || got.Revision != 2 {
		t.Fatalf("rejected plan projection=%+v", got)
	}
	if got := projection.Requests[revised.ID]; got.Status != teams.RequestApproved || got.Revision != 2 {
		t.Fatalf("approved revised plan projection=%+v", got)
	}
	if got := projection.Members[member.ID]; !got.PlanApproved || got.Status != teams.MemberIdle || got.Summary != "read-only follow-up completed" {
		t.Fatalf("member projection after approved follow-up=%+v", got)
	}
	if got := projection.Members[member.ID].Budget.AcceptedTurns; got != 3 {
		t.Fatalf("member accepted %d turns, want exactly three plan/revision/follow-up turns", got)
	}
	turns := 0
	for _, turn := range projection.Turns {
		if turn.MemberID == member.ID {
			turns++
		}
	}
	if turns != 3 {
		t.Fatalf("member has %d durable turns, want exactly three", turns)
	}
}

func assertRejectedTeamResponseDoesNotMutate(t *testing.T, model Model, project, sessionID, teamID, requestID string, revision uint64, decision string) {
	t.Helper()
	before, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(project, sessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	model.Composer.SetValue("/team " + teamID + " respond " + requestID + " " + strconv.FormatUint(revision, 10) + " " + decision + " should not persist")
	_, cmd := model.submitComposer()
	if cmd == nil {
		t.Fatalf("rejected request response %q produced no TUI command", requestID)
	}
	result, ok := cmd().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("rejected request response %q result=%T %+v; want reported error", requestID, result, result)
	}
	after, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(project, sessionID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterHistory) != len(beforeHistory) || len(afterTranscript.Events) != len(beforeTranscript.Events) {
		t.Fatalf("rejected response %q mutated durable history: team=%d→%d session=%d→%d", requestID, len(beforeHistory), len(afterHistory), len(beforeTranscript.Events), len(afterTranscript.Events))
	}
	if len(after.Requests) != len(before.Requests) || len(after.Members) != len(before.Members) {
		t.Fatalf("rejected response %q changed projection cardinality", requestID)
	}
	for id, request := range before.Requests {
		if after.Requests[id] != request {
			t.Fatalf("rejected response %q changed request %s: before=%+v after=%+v", requestID, id, request, after.Requests[id])
		}
	}
	for id, member := range before.Members {
		if !reflect.DeepEqual(after.Members[id], member) {
			t.Fatalf("rejected response %q changed member %s: before=%+v after=%+v", requestID, id, member, after.Members[id])
		}
	}
}

func pendingPlanFromTUI(t *testing.T, model Model, teamID, memberID string) teams.Request {
	t.Helper()
	_, result := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" requests")
	for _, request := range acceptanceTeamResponse(t, result, "team_request_list").TeamRequests {
		if request.Type == teams.RequestPlan && request.MemberID == memberID && request.Status == teams.RequestPending {
			return request
		}
	}
	t.Fatalf("no pending plan for member %s", memberID)
	return teams.Request{}
}

type planRevisionStage struct {
	index int
	input agent.ChildRunInput
	err   error
}

type planRevisionChildRunner struct {
	service *conversation.Service
	stages  chan planRevisionStage
	count   atomic.Int32
}

func (r *planRevisionChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	index := int(r.count.Add(1))
	stage := planRevisionStage{index: index, input: input}
	if input.TeamTurn == nil || input.ChildRunID == "" {
		stage.err = errors.New("missing team child identity")
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "missing team child identity"}
	}
	switch index {
	case 1, 2:
		body := "Initial plan: inspect parser entry point and report findings."
		if index == 2 {
			body = "Revised plan: locate parser entry point, inspect it read-only, and report findings."
		}
		_, stage.err = r.service.ExecuteTeamTool(ctx, agent.ExecutionRequest{
			RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn,
		}, llm.ToolUse{ID: "submit-plan-" + string(rune('0'+index)), Name: "team_plan_submit", Arguments: json.RawMessage(`{"team_id":"` + input.TeamTurn.TeamID + `","body":` + mustJSONForPlanRunner(body) + `}`)})
	case 3:
		stage.err = r.executeReadOnlyFollowUp(ctx, input)
	default:
		stage.err = errors.New("unexpected extra child turn")
	}
	select {
	case r.stages <- stage:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	if stage.err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: stage.err.Error()}
	}
	if index == 3 {
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "read-only follow-up completed"}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "plan submitted"}
}

func (r *planRevisionChildRunner) executeReadOnlyFollowUp(ctx context.Context, input agent.ChildRunInput) error {
	allowed := map[string]bool{}
	for _, schema := range input.ToolSchemas {
		allowed[schema.Name] = true
	}
	if !allowed["read_file"] || allowed["write_file"] || allowed["edit_file"] || allowed["command"] {
		return &readOnlyToolSchemaError{allowed: allowed}
	}
	executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn})
	if err != nil {
		return err
	}
	outcome, err := executor.Execute(ctx, llm.ToolUse{ID: "follow-up-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)})
	if err != nil {
		return err
	}
	if outcome.Status != agent.ToolSucceeded || outcome.Content != "parser entry point is parseDocument" {
		return &readOnlyToolResultError{outcome: outcome}
	}
	return nil
}

type readOnlyToolSchemaError struct{ allowed map[string]bool }

func (e *readOnlyToolSchemaError) Error() string {
	return "follow-up did not have a read-only tool allowlist"
}

type readOnlyToolResultError struct{ outcome agent.ToolOutcome }

func (e *readOnlyToolResultError) Error() string { return "read-only follow-up read did not succeed" }

func receivePlanRevisionStage(t *testing.T, stages <-chan planRevisionStage) planRevisionStage {
	t.Helper()
	select {
	case stage := <-stages:
		return stage
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for plan member stage")
		return planRevisionStage{}
	}
}

func receivePlanRevisionStageFor(t *testing.T, stages <-chan planRevisionStage, root, sessionID, teamID, memberID string) planRevisionStage {
	t.Helper()
	select {
	case stage := <-stages:
		return stage
	case <-time.After(5 * time.Second):
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err != nil {
			t.Fatalf("timed out waiting for revised plan stage; projection read failed: %v", err)
		}
		t.Fatalf("timed out waiting for revised plan stage; member=%+v requests=%+v", projection.Members[memberID], projection.Requests)
		return planRevisionStage{}
	}
}

func waitPlanRevisionMemberStatus(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
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
		t.Fatal(err)
	}
	t.Fatalf("member status=%q, want %q; member=%+v turns=%+v requests=%+v", projection.Members[memberID].Status, want, projection.Members[memberID], projection.Turns, projection.Requests)
}

func mustJSONForPlanRunner(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
