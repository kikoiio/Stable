package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberMessageToLeadPersistsWithoutStartingLeadRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "member-message-lead")
	team, err := service.CreateTeam(context.Background(), leadRequest, "member-message")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, leadRequest, team.ID, "member-message-reader", "reader")

	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	memberRequest := leadRequest
	memberRequest.RunID = "member-message-child"
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: "member-message-reader", TurnID: turnID, MemberName: "reader"}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberRequest.TeamTurn.MemberID, RunID: memberRequest.RunID, TaskID: turnID, OriginRunID: leadRequest.RunID, Status: "intent"}
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: memberRequest.RunID, WorkKind: string(leadRequest.Work.Kind), Intent: "send findings to lead",
		TeamID: team.ID, TeamMemberID: memberRequest.TeamTurn.MemberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{
		"team_id":   team.ID,
		"recipient": teams.Lead,
		"body":      "Parser recovery is verified.",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.ExecuteTeamTool(context.Background(), memberRequest, llm.ToolUse{
		ID: "member-to-lead", Name: "team_send", Arguments: arguments,
	})
	if err != nil || outcome.Status != agent.ToolSucceeded || outcome.IsError {
		t.Fatalf("member team_send to lead = %+v, %v", outcome, err)
	}

	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	var message teams.Message
	for _, candidate := range projection.Messages {
		if candidate.SenderID == memberRequest.TeamTurn.MemberID {
			message = candidate
		}
	}
	if message.ID == "" || message.Body != "Parser recovery is verified." || message.SenderID != memberRequest.TeamTurn.MemberID || len(message.Recipients) != 1 || message.Recipients[0] != teams.Lead {
		t.Fatalf("member-to-lead message was not durably recorded with a fixed lead recipient: %+v", message)
	}
	leadMessages, err := service.ListTeamMessages(context.Background(), leadRequest, team.ID, 0, teams.MaxPageSize)
	if err != nil || len(leadMessages) != 1 || leadMessages[0].ID != message.ID {
		t.Fatalf("lead cannot read its durable notification: messages=%+v err=%v", leadMessages, err)
	}

	after, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeLeadStarts, afterLeadStarts int
	for _, event := range before.Events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if err := decodeSessionData(event.Data, &started); err != nil {
				t.Fatal(err)
			}
			if started.RunID == leadRequest.RunID {
				beforeLeadStarts++
			}
		}
	}
	for _, event := range after.Events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if err := decodeSessionData(event.Data, &started); err != nil {
				t.Fatal(err)
			}
			if started.RunID == leadRequest.RunID {
				afterLeadStarts++
			}
		}
	}
	if beforeLeadStarts != 1 || afterLeadStarts != beforeLeadStarts {
		t.Fatalf("member message started or duplicated lead run: starts before=%d after=%d", beforeLeadStarts, afterLeadStarts)
	}
}
