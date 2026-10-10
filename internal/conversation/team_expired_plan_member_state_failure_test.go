package conversation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestExpiredPlanMemberStateAppendFailureRetriesFromDurableExpiry(t *testing.T) {
	testExpiredPlanMemberStateAppendFailure(t, agent.DelegationSucceeded, teams.MemberIdle)
}

func TestFailedExpiredPlanMemberStateAppendFailureRetriesFromDurableExpiry(t *testing.T) {
	testExpiredPlanMemberStateAppendFailure(t, agent.DelegationFailed, teams.MemberInterrupted)
}

func testExpiredPlanMemberStateAppendFailure(t *testing.T, childStatus agent.DelegationStatus, expectedMemberStatus teams.MemberStatus) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "expired-plan-state-failure-parent")
	team, err := service.CreateTeam(t.Context(), request, "expired-plan-state-failure")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-expired-plan-state-failure", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, PlanRequired: true, Revision: 1,
	}
	appendFact := func(fact sessionlog.TeamEvent) {
		t.Helper()
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, fact); err != nil {
			t.Fatal(err)
		}
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member})
	turn := sessionlog.TurnFact{
		ID: "turn-expired-plan-state-failure", MemberID: member.ID, RunID: "child-expired-plan-state-failure", TaskID: "task-expired-plan-state-failure",
		OriginRunID: request.RunID, OriginCallID: "spawn-expired-plan-state-failure", Status: "intent",
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn})
	accepted := turn
	accepted.Status = "queued"
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted})
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-expired-plan-state-failure", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	seq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	team, err = service.GetTeam(t.Context(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(40 * time.Millisecond)
	plan, err := service.createTeamRequestUntil(root, team, child.ChildRunID, member.ID, member.ID, teams.RequestPlan, "inspect", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expiresAt) + time.Millisecond)
	if err := service.persistTeamChildFinishLocked(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child,
		agent.ChildRunResult{Status: childStatus, Summary: "plan requested", Error: map[bool]string{true: "injected child failure"}[childStatus == agent.DelegationFailed]}, &seq); err != nil {
		t.Fatal(err)
	}
	terminal := accepted
	terminal.Status = string(childStatus)
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: request.RunID, Turn: &terminal})
	if childStatus == agent.DelegationSucceeded {
		projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		member = projection.Members[member.ID]
		member.Status, member.Revision = teams.MemberAwaitingPlan, member.Revision+1
		appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member})
	}

	appendFailure := errors.New("injected expired-plan member-state append failure")
	stateAppends := 0
	service.teamMemberStateAppender = func(string, teams.Team, string, string, teams.Member) error {
		stateAppends++
		return appendFailure
	}
	if _, err := service.ListTeamRequests(t.Context(), request, team.ID); !errors.Is(err, appendFailure) {
		t.Fatalf("first request query error=%v, want injected member-state failure", err)
	}
	if stateAppends != 1 {
		t.Fatalf("injected member-state append calls=%d, want one", stateAppends)
	}
	failed, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := failed.Requests[plan.ID].Status; got != teams.RequestExpired {
		t.Fatalf("durable expired request status=%s, want expired", got)
	}
	initialMemberStatus := teams.MemberRunning
	if childStatus == agent.DelegationSucceeded {
		initialMemberStatus = teams.MemberAwaitingPlan
	}
	if got := failed.Members[member.ID].Status; got != initialMemberStatus {
		t.Fatalf("failed state append published member status=%s, want %s", got, initialMemberStatus)
	}

	service.teamMemberStateAppender = nil
	if _, err := service.ListTeamRequests(t.Context(), request, team.ID); err != nil {
		t.Fatalf("request query retry did not reconcile durable expiry: %v", err)
	}
	recovered, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Members[member.ID].Status; got != expectedMemberStatus {
		t.Fatalf("retried member status=%s, want %s", got, expectedMemberStatus)
	}
	if got := recovered.Requests[plan.ID].Status; got != teams.RequestExpired {
		t.Fatalf("retry changed durable request status=%s, want expired", got)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)
	if _, err := service.ListTeamRequests(t.Context(), request, team.ID); err != nil {
		t.Fatalf("second reconciled query: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("idempotent query appended events: %d -> %d", eventCount, len(transcript.Events))
	}
	if got := recoveryTeamTurnTerminalCount(t, root, request.Work.SessionID, team.ID, turn.ID); got != 1 {
		t.Fatalf("durable child turn terminal count=%d, want one", got)
	}
}
