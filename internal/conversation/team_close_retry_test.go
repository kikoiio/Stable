package conversation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCloseTeamRetriesIdleMemberStateAppendInProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "close-retry-parent")
	team, err := service.CreateTeam(t.Context(), request, "close-retry")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-close-retry", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}

	writeFailure := errors.New("injected idle member-state append failure")
	appendAttempts := 0
	service.teamMemberStateAppender = func(appendRoot string, appendTeam teams.Team, runID, actor string, stopped teams.Member) error {
		appendAttempts++
		if appendAttempts == 1 {
			return writeFailure
		}
		return appendTeamFactLocked(appendRoot, appendTeam.Scope.SessionID, appendTeam.ID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamMemberState, ActorID: actor, ActorRunID: runID, Member: &stopped,
		})
	}

	closing, err := service.CloseTeam(t.Context(), request, team.ID)
	if !errors.Is(err, writeFailure) || closing.Status != teams.TeamClosing {
		t.Fatalf("first close = %+v, %v; want durable closing state and injected append error", closing, err)
	}
	afterFailure, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Teams[team.ID].Status != teams.TeamClosing || afterFailure.Members[member.ID].Status != teams.MemberCreated {
		t.Fatalf("failed close projection = team %s/member %s; want closing/created", afterFailure.Teams[team.ID].Status, afterFailure.Members[member.ID].Status)
	}

	closed, err := service.CloseTeam(t.Context(), request, team.ID)
	if err != nil || closed.Status != teams.TeamClosed {
		t.Fatalf("retry close = %+v, %v; want closed", closed, err)
	}
	final, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Teams[team.ID].Status != teams.TeamClosed || final.Members[member.ID].Status != teams.MemberStopped {
		t.Fatalf("final projection = team %s/member %s; want closed/stopped", final.Teams[team.ID].Status, final.Members[member.ID].Status)
	}
	if appendAttempts != 2 {
		t.Fatalf("member-state append attempts=%d, want one failure and one retry", appendAttempts)
	}

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	closingFacts, memberStateFacts, closedFacts := 0, 0, 0
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if decodeSessionData(event.Data, &fact) != nil || fact.TeamID != team.ID {
			continue
		}
		switch fact.Kind {
		case sessionlog.TeamClosing:
			closingFacts++
		case sessionlog.TeamMemberState:
			if fact.Member != nil && fact.Member.ID == member.ID {
				memberStateFacts++
			}
		case sessionlog.TeamClosed:
			closedFacts++
		}
	}
	if closingFacts != 1 || memberStateFacts != 1 || closedFacts != 1 {
		t.Fatalf("close facts = closing/member-state/closed %d/%d/%d; want exactly one each", closingFacts, memberStateFacts, closedFacts)
	}
}
