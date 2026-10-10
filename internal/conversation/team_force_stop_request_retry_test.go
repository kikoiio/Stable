package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestForceStopRetriesApprovedDeferredRequestWithoutDuplicateHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "force-stop-request-retry-parent")
	team, err := service.CreateTeam(t.Context(), request, "force-stop-request-retry")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "force-stop-request-retry-member", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "force-stop-request-retry-turn", MemberID: member.ID, RunID: "force-stop-request-retry-child", TaskID: "force-stop-request-retry-task", OriginRunID: request.RunID, OriginCallID: "force-stop-request-retry-spawn", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{TeamTurn: &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name}, ParentRunID: request.RunID, BatchID: "force-stop-request-retry-batch", ChildRunID: turn.RunID, Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work}
	seq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := service.persistTeamChildStart(root, team.Scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	childRequest := agent.ExecutionRequest{RunID: turn.RunID, Work: request.Work, TeamTurn: child.TeamTurn}
	shutdown, err := service.RequestTeamShutdown(t.Context(), request, team.ID, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := service.RespondTeamRequest(t.Context(), childRequest, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), "Finish the current task first.")
	if err != nil || deferred.Status != teams.RequestDeferred {
		t.Fatalf("defer shutdown = %+v, %v", deferred, err)
	}

	schedulerCtx, cancelScheduler := context.WithCancel(context.Background())
	service.lifeCtx = schedulerCtx
	service.teamScheduler = newTeamScheduler(service)
	cancelCalls := 0
	service.teamScheduler.active[turn.ID] = func() { cancelCalls++ }
	service.teamScheduler.activeMember[turn.ID] = member.ID
	service.teamScheduler.activeTeam[turn.ID] = team.ID
	t.Cleanup(func() {
		cancelScheduler()
		service.teamScheduler.close()
	})

	writeFailure := errors.New("injected force-stop MemberState failure")
	service.teamMemberStateAppender = func(string, teams.Team, string, string, teams.Member) error { return writeFailure }
	if _, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, member.ID); !errors.Is(err, writeFailure) {
		t.Fatalf("first force stop error=%v, want injected member state failure", err)
	}
	if cancelCalls != 0 {
		t.Fatalf("child cancel calls after failed durable stop=%d, want zero", cancelCalls)
	}
	failed, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Members[member.ID].Status != teams.MemberRunning || failed.Requests[shutdown.ID].Status != teams.RequestApproved {
		t.Fatalf("failed stop projection member=%s request=%s", failed.Members[member.ID].Status, failed.Requests[shutdown.ID].Status)
	}
	service.teamMemberStateAppender = nil
	stopped, err := service.StopTeamMember(t.Context(), request.Work.SessionID, team.ID, member.ID)
	if err != nil || stopped.Status != teams.MemberStopping {
		t.Fatalf("retry force stop = %+v, %v", stopped, err)
	}
	if cancelCalls != 1 {
		t.Fatalf("child cancel calls after retry=%d, want one", cancelCalls)
	}
	recovered, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Requests) != 1 || recovered.Requests[shutdown.ID].Status != teams.RequestApproved || recovered.Requests[shutdown.ID].Revision != 3 || recovered.Members[member.ID].Status != teams.MemberStopping {
		t.Fatalf("retry duplicated force-stop history or failed to stop: requests=%+v member=%+v", recovered.Requests, recovered.Members[member.ID])
	}
}
