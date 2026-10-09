package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTwoTeamMembersRaceToClaimTaskOnlyOneSucceeds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "task-claim-race-lead")
	team, err := service.CreateTeam(context.Background(), leadRequest, "claim-race")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTeamTask(context.Background(), leadRequest, team.ID, teams.Task{Title: "shared claim"})
	if err != nil {
		t.Fatal(err)
	}

	requests := make([]agent.ExecutionRequest, 0, 2)
	for _, memberID := range []string{"claim-member-a", "claim-member-b"} {
		addTeamMessageMember(t, service, leadRequest, team.ID, memberID, memberID)
		turnID, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		childRunID, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		turn := sessionlog.TurnFact{
			ID: turnID, MemberID: memberID, RunID: childRunID, TaskID: turnID,
			OriginRunID: leadRequest.RunID, Status: "intent",
		}
		appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
		turn.Status = "queued"
		appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
		if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: childRunID, WorkKind: string(leadRequest.Work.Kind), Intent: "claim race fixture",
			TeamID: team.ID, TeamMemberID: memberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
		}); err != nil {
			t.Fatal(err)
		}
		request := leadRequest
		request.RunID = childRunID
		request.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: memberID, TurnID: turnID, MemberName: memberID}
		requests = append(requests, request)
	}

	before, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		request agent.ExecutionRequest
		task    teams.Task
		err     error
	}
	results := make(chan result, len(requests))
	var ready sync.WaitGroup
	ready.Add(len(requests))
	for _, request := range requests {
		request := request
		go func() {
			ready.Done()
			<-start
			assignee := request.TeamTurn.MemberID
			claimed, claimErr := service.UpdateTeamTask(context.Background(), request, team.ID, task.ID, task.Revision, teams.TaskPatch{Assignee: &assignee})
			results <- result{request: request, task: claimed, err: claimErr}
		}()
	}
	ready.Wait()
	close(start)

	wins, conflicts := 0, 0
	var winner string
	for range requests {
		got := <-results
		switch {
		case got.err == nil:
			wins++
			winner = got.request.TeamTurn.MemberID
			if got.task.Assignee != winner || got.task.Revision != task.Revision+1 {
				t.Fatalf("successful claim=%+v, want assignee %s at revision %d", got.task, winner, task.Revision+1)
			}
		case errors.Is(got.err, teams.ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("claim by %s failed with %v; want success or revision conflict", got.request.TeamTurn.MemberID, got.err)
		}
	}
	if wins != 1 || conflicts != 1 || winner == "" {
		t.Fatalf("claim race results: wins=%d conflicts=%d winner=%q; want exactly one of each", wins, conflicts, winner)
	}

	final, err := service.GetTeamTask(context.Background(), leadRequest, team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Assignee != winner || final.Revision != task.Revision+1 {
		t.Fatalf("final claim projection=%+v, want winner %s at revision %d", final, winner, task.Revision+1)
	}
	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable := projection.Tasks[task.ID]; durable.Assignee != winner || durable.Revision != task.Revision+1 {
		t.Fatalf("durable claim=%+v, want winner %s at revision %d", durable, winner, task.Revision+1)
	}
	transcript, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(transcript.Events); got != len(before.Events)+1 {
		t.Fatalf("claim race appended %d events, want exactly one (before=%d after=%d)", got-len(before.Events), len(before.Events), got)
	}
}
