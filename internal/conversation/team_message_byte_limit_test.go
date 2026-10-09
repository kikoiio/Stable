package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMessageByteLimitForLeadAndMemberIsAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "message-limit-lead")
	team, err := service.CreateTeam(context.Background(), leadRequest, "message-limit")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, leadRequest, team.ID, "message-limit-member", "reader")

	memberRequest := leadRequest
	memberRequest.RunID = "message-limit-child"
	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: "message-limit-member", TurnID: turnID, MemberName: "reader"}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberRequest.TeamTurn.MemberID, RunID: memberRequest.RunID, TaskID: turnID, OriginRunID: leadRequest.RunID, Status: "intent"}
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: memberRequest.RunID, WorkKind: string(leadRequest.Work.Kind), Intent: "message byte boundary",
		TeamID: team.ID, TeamMemberID: memberRequest.TeamTurn.MemberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}

	for _, actor := range []struct {
		name    string
		request agent.ExecutionRequest
		to      string
	}{
		{name: "lead", request: leadRequest, to: "message-limit-member"},
		{name: "member", request: memberRequest, to: teams.Lead},
	} {
		t.Run(actor.name, func(t *testing.T) {
			body := strings.Repeat("x", teams.MaxMessageBytes)
			if got := len([]byte(body)); got != teams.MaxMessageBytes {
				t.Fatalf("fixture body is %d bytes, want %d", got, teams.MaxMessageBytes)
			}
			accepted, err := service.SendTeamMessage(context.Background(), actor.request, TeamSendRequest{
				TeamID: team.ID, Recipient: actor.to, Body: body, Token: actor.name + "-max",
			})
			if err != nil {
				t.Fatalf("exact maximum body rejected: %v", err)
			}
			if got := len([]byte(accepted.Body)); got != teams.MaxMessageBytes {
				t.Fatalf("persisted body is %d bytes, want %d", got, teams.MaxMessageBytes)
			}

			before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			factCount := len(teamMessageFacts(t, service, leadRequest, team.ID))
			oversize := body + "x"
			if _, err := service.SendTeamMessage(context.Background(), actor.request, TeamSendRequest{
				TeamID: team.ID, Recipient: actor.to, Body: oversize, Token: actor.name + "-over",
			}); err == nil {
				t.Fatal("body one byte over maximum was accepted")
			}
			after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(teamMessageFacts(t, service, leadRequest, team.ID)); got != factCount {
				t.Fatalf("rejected message appended a fact: before=%d after=%d", factCount, got)
			}
			if after.LastSeq[team.ID] != before.LastSeq[team.ID] || after.Teams[team.ID].Revision != before.Teams[team.ID].Revision || len(after.Messages) != len(before.Messages) {
				t.Fatalf("rejected message changed durable counters: before seq/rev/messages=%d/%d/%d after=%d/%d/%d", before.LastSeq[team.ID], before.Teams[team.ID].Revision, len(before.Messages), after.LastSeq[team.ID], after.Teams[team.ID].Revision, len(after.Messages))
			}
			beforeMember := before.Members["message-limit-member"]
			afterMember := after.Members["message-limit-member"]
			if afterMember.Budget != beforeMember.Budget {
				t.Fatalf("rejected message changed member budget: before=%+v after=%+v", beforeMember.Budget, afterMember.Budget)
			}
			// The rejected token must remain available for a valid request.
			if _, err := service.SendTeamMessage(context.Background(), actor.request, TeamSendRequest{
				TeamID: team.ID, Recipient: actor.to, Body: body, Token: actor.name + "-over",
			}); err != nil {
				t.Fatalf("rejected oversize request consumed its token: %v", err)
			}
		})
	}
}

func appendTeamMessageLimitFact(t *testing.T, service *Service, request agent.ExecutionRequest, teamID, kind string, turn sessionlog.TurnFact) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, request.Work.SessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(service.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: id, TeamID: teamID, SessionID: request.Work.SessionID, Kind: kind,
		Revision: projection.Teams[teamID].Revision + 1, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
}
