package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamPlanApprovalAutomaticallyStartsReadOnlyFollowUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "plan-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 2), release: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ToolSchemas = []llm.ToolSchema{
		{Name: "read_file"}, {Name: "write_file"}, {Name: "command"},
		{Name: "team_plan_submit"}, {Name: "team_request_list"}, {Name: "team_send"}, {Name: "team_member_spawn"},
	}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "plan-approval")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "call-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("unexpected initial child turn: %+v", first.TeamTurn)
	}
	assertTeamPlanToolSchemas(t, first.ToolSchemas)

	childRequest := agent.ExecutionRequest{RunID: first.ChildRunID, Work: request.Work, TeamTurn: first.TeamTurn}
	planCall := llm.ToolUse{
		ID: "call-submit-plan", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + team.ID + `","body":"Inspect the parser and report findings."}`),
	}
	planOutcome, err := service.ExecuteTeamTool(t.Context(), childRequest, planCall)
	if err != nil || planOutcome.Status != agent.ToolSucceeded {
		t.Fatalf("member plan submission outcome=%+v err=%v", planOutcome, err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var submitted teams.Request
	for _, fact := range projection.Requests {
		if fact.MemberID == member.ID && fact.Type == teams.RequestPlan {
			submitted = fact
		}
	}
	if submitted.ID == "" || submitted.Status != teams.RequestPending || submitted.Revision != 1 {
		t.Fatalf("member plan request = %+v, want one pending revision-1 request", submitted)
	}
	if _, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, submitted.ID, submitted.Revision, string(teams.RequestApproved), ""); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("member responded to its own plan request: %v", err)
	}
	if _, err := service.RespondTeamRequest(t.Context(), request, team.ID, submitted.ID, submitted.Revision+1, string(teams.RequestApproved), ""); !errors.Is(err, teams.ErrRevisionConflict) {
		t.Fatalf("stale plan response error = %v, want revision conflict", err)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberAwaitingPlan)
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "call-resume-before-approval"); err == nil {
		t.Fatal("unapproved plan-required member resumed")
	}
	if runner.childCount() != 1 {
		t.Fatalf("unapproved member started %d child turns, want only the initial turn", runner.childCount())
	}
	approved, err := service.RespondTeamRequest(t.Context(), request, team.ID, submitted.ID, submitted.Revision, string(teams.RequestApproved), "Proceed with the read-only review.")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != teams.RequestApproved || approved.Revision != submitted.Revision+1 {
		t.Fatalf("approved request = %+v", approved)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	memberState := projection.Members[member.ID]
	if !memberState.PlanApproved || memberState.Status != teams.MemberIdle && memberState.Status != teams.MemberQueued && memberState.Status != teams.MemberRunning || len(memberState.Tools) != 1 || memberState.Tools[0] != "read_file" {
		t.Fatalf("plan approval changed member state or tool allowlist unexpectedly: %+v", memberState)
	}
	second := receiveTeamChildInput(t, runner.inputs)
	if second.TeamTurn == nil || second.TeamTurn.MemberID != member.ID || second.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("plan approval did not start a fresh turn for the same member: first=%+v second=%+v", first.TeamTurn, second.TeamTurn)
	}
	assertTeamPlanToolSchemas(t, second.ToolSchemas)
	if runner.childCount() != 2 {
		t.Fatalf("plan approval started %d total turns, want two", runner.childCount())
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
}

func TestExpiredTeamRequestIsPersistedAndCannotBeAnswered(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "request-expiry-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ToolSchemas = []llm.ToolSchema{{Name: "team_plan_submit"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "request-expiry")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", PlanRequired: true, OriginCallID: "call-expiry",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := receiveTeamChildInput(t, runner.inputs)
	team, err = service.GetTeam(t.Context(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(50 * time.Millisecond)
	pending, err := service.createTeamRequestUntil(root, team, child.ChildRunID, member.ID, member.ID, teams.RequestPlan, "inspect", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	team.Revision++
	shutdown, err := service.createTeamRequestUntil(root, team, request.RunID, teams.Lead, member.ID, teams.RequestShutdown, "", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expiresAt) + 10*time.Millisecond)

	expired, err := service.RespondTeamRequest(t.Context(), request, team.ID, pending.ID, pending.Revision, string(teams.RequestApproved), "")
	if err == nil || expired.Status != teams.RequestExpired || expired.Revision != pending.Revision+1 {
		t.Fatalf("expired direct response = %+v, err=%v", expired, err)
	}
	listed, err := service.ListTeamRequests(t.Context(), request, team.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("expired request listing = %+v, err=%v", listed, err)
	}
	for _, item := range listed {
		if item.Status != teams.RequestExpired || item.Revision != 2 || item.ID != pending.ID && item.ID != shutdown.ID {
			t.Fatalf("request did not reach its expired revision: %+v", item)
		}
	}
	listed, err = service.ListTeamRequests(t.Context(), request, team.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("repeated request listing changed expiry state: %+v, err=%v", listed, err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiredEvents := 0
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		raw, marshalErr := json.Marshal(event.Data)
		if marshalErr == nil && json.Unmarshal(raw, &fact) == nil && fact.Kind == sessionlog.TeamRequestExpired && fact.Request != nil {
			if fact.Request.ID == pending.ID || fact.Request.ID == shutdown.ID {
				expiredEvents++
			}
		}
	}
	if projection.Requests[pending.ID].Status != teams.RequestExpired || projection.Requests[shutdown.ID].Status != teams.RequestExpired || expiredEvents != 2 {
		t.Fatalf("request expiration replay=%+v/%+v, expiration events=%d", projection.Requests[pending.ID], projection.Requests[shutdown.ID], expiredEvents)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberAwaitingPlan)
}

func assertTeamPlanToolSchemas(t *testing.T, schemas []llm.ToolSchema) {
	t.Helper()
	allowed := map[string]bool{}
	for _, schema := range schemas {
		allowed[schema.Name] = true
	}
	if !allowed["read_file"] {
		t.Fatalf("role inspection tool missing from member schemas: %+v", schemas)
	}
	for _, forbidden := range []string{"write_file", "command", "team_member_spawn"} {
		if allowed[forbidden] {
			t.Fatalf("plan approval exposed forbidden tool %q: %+v", forbidden, schemas)
		}
	}
}
