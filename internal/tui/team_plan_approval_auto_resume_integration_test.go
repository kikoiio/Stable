package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// Approval through the user-facing TUI must resume a genuinely paused member
// without requiring a second, explicit /resume command.
func TestTeamPlanApprovalTUIAutomaticallyResumesReadOnlyMember(t *testing.T) {
	ctx := context.Background()
	tmp := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmp, "tui-plan-auto-resume-")
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

	child := &autoResumePlanChildRunner{stages: make(chan autoResumePlanStage, 3)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), child, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parent := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "conversation.sock")
	readExecutor := &agent.FakeExecutor{Script: []agent.ToolOutcome{{Content: "parser entry point is parseDocument"}}}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parent, Delegator: pool, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: readExecutor},
		ToolSchemas:         []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}, {Name: "edit_file"}, {Name: "command"}, {Name: "team_plan_submit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	child.service = svc
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "approve a paused plan member",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parent.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create plan-auto-resume")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect parser --plan")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil || !member.PlanRequired {
		t.Fatalf("spawn response=%+v, want plan-required member", member)
	}
	first := receiveAutoResumePlanStage(t, child.stages)
	if first.err != nil || first.input.TeamTurn == nil || first.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("plan submission stage=%+v", first)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberAwaitingPlan)

	model.ActiveRunID = "" // the lead response is a trusted user action in this team scope.
	_, requestsResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	var pending teams.Request
	for _, request := range acceptanceTeamResponse(t, requestsResult, "team_request_list").TeamRequests {
		if request.Type == teams.RequestPlan && request.MemberID == member.ID && request.Status == teams.RequestPending {
			pending = request
		}
	}
	if pending.ID == "" || pending.Revision != 1 || pending.RequesterID != member.ID || pending.ResponderID != teams.Lead {
		t.Fatalf("pending plan=%+v, want member requester and lead responder", pending)
	}
	_, responseResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" respond "+pending.ID+" 1 approve Continue with read-only inspection.")
	approved := acceptanceTeamResponse(t, responseResult, "team_request_respond").TeamRequest
	if approved == nil || approved.Status != teams.RequestApproved || approved.Revision != 2 {
		t.Fatalf("approval response=%+v", approved)
	}

	// Deliberately do not issue /resume: approval itself must wake the paused child.
	followUp := receiveAutoResumePlanStage(t, child.stages)
	if followUp.err != nil || followUp.input.TeamTurn == nil || followUp.input.TeamTurn.MemberID != member.ID {
		t.Fatalf("automatic follow-up stage=%+v", followUp)
	}
	if first.input.ChildRunID == followUp.input.ChildRunID || first.input.TeamTurn.TurnID == followUp.input.TeamTurn.TurnID {
		t.Fatalf("follow-up reused plan turn identity: first=%+v follow-up=%+v", first.input, followUp.input)
	}
	waitPlanRevisionMemberStatus(t, project, sessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := projection.Members[member.ID]
	if !got.PlanApproved || got.Status != teams.MemberIdle || got.Summary != "approved read-only inspection completed" {
		t.Fatalf("member after automatic approved follow-up=%+v", got)
	}
	if turns := len(projection.Turns); turns != 2 {
		t.Fatalf("durable team turns=%d, want plan and exactly one approved follow-up", turns)
	}
	if calls := child.calls.Load(); calls != 2 {
		t.Fatalf("child runner calls=%d, want exactly two without duplicate resume", calls)
	}
}

type autoResumePlanStage struct {
	input agent.ChildRunInput
	err   error
}

type autoResumePlanChildRunner struct {
	service *conversation.Service
	stages  chan autoResumePlanStage
	calls   atomic.Int32
}

func (r *autoResumePlanChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	stage := autoResumePlanStage{input: input}
	index := r.calls.Add(1)
	if input.TeamTurn == nil || input.ChildRunID == "" {
		stage.err = errors.New("missing team child identity")
	} else if index == 1 {
		payload, _ := json.Marshal(map[string]string{"team_id": input.TeamTurn.TeamID, "body": "Plan: inspect parser entry point read-only and report findings."})
		_, stage.err = r.service.ExecuteTeamTool(ctx, agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn}, llm.ToolUse{ID: "submit-initial-plan", Name: "team_plan_submit", Arguments: payload})
	} else if index == 2 {
		stage.err = r.executeReadOnly(ctx, input)
	} else {
		stage.err = errors.New("unexpected duplicate child turn")
	}
	select {
	case r.stages <- stage:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	if stage.err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: stage.err.Error()}
	}
	if index == 2 {
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "approved read-only inspection completed"}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "plan submitted"}
}

func (r *autoResumePlanChildRunner) executeReadOnly(ctx context.Context, input agent.ChildRunInput) error {
	allowed := map[string]bool{}
	for _, schema := range input.ToolSchemas {
		allowed[schema.Name] = true
	}
	if !allowed["read_file"] || allowed["write_file"] || allowed["edit_file"] || allowed["command"] {
		return errors.New("approved follow-up did not receive a read-only tool allowlist")
	}
	executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn})
	if err != nil {
		return err
	}
	outcome, err := executor.Execute(ctx, llm.ToolUse{ID: "approved-follow-up-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)})
	if err != nil {
		return err
	}
	if outcome.Status != agent.ToolSucceeded || !strings.Contains(outcome.Content, "parser entry point is parseDocument") {
		return errors.New("approved read-only follow-up did not complete the expected read")
	}
	return nil
}

func receiveAutoResumePlanStage(t *testing.T, stages <-chan autoResumePlanStage) autoResumePlanStage {
	t.Helper()
	select {
	case stage := <-stages:
		return stage
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for plan member stage")
		return autoResumePlanStage{}
	}
}
