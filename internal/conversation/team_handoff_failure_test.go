package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMessageHandoffAppendFailureCompensatesAndAllowsRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "parent-run")
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	team, err := service.CreateTeam(t.Context(), request, "handoff-write-failure")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, team.ID, "member-handoff-failure", "reader")
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members["member-handoff-failure"]
	member.Status = teams.MemberInterrupted
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	message, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "resume with the parser edge case", Token: "handoff-failure-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	request.AcceptTeamRoleChange = true // The fixture member has a synthetic role hash.
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 3)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	// The member is interrupted and the message predates scheduler binding, so
	// this test's first admission is the explicit retry under fault injection.
	scheduler := service.teamScheduler
	if _, _, actor, scopeErr := service.teamOperationScope(t.Context(), request); scopeErr != nil || !actor.Lead {
		t.Fatalf("test lead operation is not authorized: actor=%+v err=%v", actor, scopeErr)
	}
	if _, scope, scopeErr := service.teamScope(t.Context(), request); scopeErr != nil {
		t.Fatalf("test lead scope is not authorized: %v", scopeErr)
	} else if err := validateTeamLeadAuthority(request, scope); err != nil {
		t.Fatalf("test request authority is invalid: %v", err)
	}
	handoffAppendCalls := 0
	scheduler.appendHandoff = func(string, string, string, sessionlog.HandoffFact) error {
		handoffAppendCalls++
		return errors.New("injected handoff event append failure")
	}
	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-failed-handoff"); err == nil || !strings.Contains(err.Error(), "injected handoff event append failure") {
		t.Fatalf("resume after injected handoff persistence failure returned %v (appender calls=%d)", err, handoffAppendCalls)
	}
	if handoffAppendCalls != 1 {
		t.Fatalf("handoff appender called %d times, want exactly once", handoffAppendCalls)
	}
	if runner.count() != 0 {
		t.Fatalf("provider started despite failed handoff append: child runs=%d", runner.count())
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Handoffs) != 0 {
		t.Fatalf("failed handoff was reported durable: %+v", projection.Handoffs)
	}
	var failedTurn sessionlog.TurnFact
	for _, turn := range projection.Turns {
		if turn.ID != member.TurnID && containsString(turn.MessageIDs, message.ID) {
			failedTurn = turn
		}
	}
	if failedTurn.ID == "" || failedTurn.Status != string(agent.DelegationInterrupted) {
		t.Fatalf("unpublished turn was not durably compensated: %+v", failedTurn)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("member status after failed handoff = %s, want interrupted", got)
	}

	scheduler.appendHandoff = nil
	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "retry-handoff"); err != nil {
		t.Fatalf("explicit retry after handoff failure: %v", err)
	}
	retry := receiveTeamChildInput(t, runner.inputs)
	if retry.TeamTurn == nil || strings.Count(retry.Task.Instruction, message.Body) != 1 {
		t.Fatalf("retry did not receive one copy of unhanded message: %+v", retry)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Handoffs) != 1 || projection.Handoffs[0].MessageID != message.ID || projection.Handoffs[0].DestinationTurnID != retry.TeamTurn.TurnID {
		t.Fatalf("successful retry handoff chain=%+v", projection.Handoffs)
	}
	if runner.count() != 1 {
		t.Fatalf("child runner count after explicit retry=%d, want 1", runner.count())
	}
}
