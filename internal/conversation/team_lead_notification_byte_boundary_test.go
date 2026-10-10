package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamLeadNotificationBatchEnforcesExactByteBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "lead-byte-boundary-origin")
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	team, err := service.CreateTeam(t.Context(), leadRequest, "lead-byte-boundary")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "lead-byte-boundary-member"
	addTeamMessageMember(t, service, leadRequest, team.ID, memberID, "reader")
	memberRequest := appendLeadNotificationTurn(t, service, leadRequest, team.ID, memberID, "lead-byte-boundary-child")

	// Four maximum-size messages fill the lead handoff batch exactly. The next
	// valid message is one byte over the aggregate limit and must stay pending.
	messageIDs := make([]string, 0, teams.MaxBatchBytes/teams.MaxMessageBytes+1)
	for i := 0; i < teams.MaxBatchBytes/teams.MaxMessageBytes; i++ {
		message, sendErr := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
			TeamID: team.ID, Recipient: teams.Lead,
			Body: strings.Repeat("x", teams.MaxMessageBytes), Token: "lead-byte-boundary-max-" + string(rune('a'+i)),
		})
		if sendErr != nil {
			t.Fatalf("send exact-boundary message %d: %v", i, sendErr)
		}
		messageIDs = append(messageIDs, message.ID)
	}
	overflow, err := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
		TeamID: team.ID, Recipient: teams.Lead, Body: "y", Token: "lead-byte-boundary-plus-one",
	})
	if err != nil {
		t.Fatalf("send valid one-byte overflow message: %v", err)
	}
	messageIDs = append(messageIDs, overflow.ID)

	runner := startCapturedParent(t, service, leadRequest, "lead-byte-boundary-next")
	var notices []teamLeadNotice
	for _, input := range runner.request.Messages {
		if !strings.HasPrefix(input.Content, "Team notifications (") {
			continue
		}
		encoded := input.Content[strings.IndexByte(input.Content, '\n')+1:]
		if err := json.Unmarshal([]byte(encoded), &notices); err != nil {
			t.Fatalf("decode lead notifications: %v", err)
		}
	}
	var deliveredBytes int
	for i, notice := range notices {
		if i >= len(messageIDs)-1 || notice.MessageID != messageIDs[i] {
			t.Fatalf("lead batch message %d = %q, want ordered exact-boundary message %q", i, notice.MessageID, messageIDs[i])
		}
		deliveredBytes += len([]byte(notice.Body))
	}
	if len(notices) != len(messageIDs)-1 || deliveredBytes != teams.MaxBatchBytes {
		t.Fatalf("lead batch contained %d messages / %d body bytes, want %d / %d", len(notices), deliveredBytes, len(messageIDs)-1, teams.MaxBatchBytes)
	}
	for _, input := range runner.request.Messages {
		if strings.Contains(input.Content, overflow.ID) || strings.Contains(input.Content, "\"body\":\"y\"") {
			t.Fatalf("one-byte-overflow message leaked into parent messages: %+v", input)
		}
	}

	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, pending := projection.Messages[overflow.ID]; !pending {
		t.Fatalf("one-byte-overflow message was consumed instead of remaining pending: %+v", projection.Messages)
	}
	history, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handoffs := 0
	for _, event := range history {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind != sessionlog.TeamLeadHandoff || fact.Handoff == nil || fact.Handoff.DestinationRunID != "lead-byte-boundary-next" {
			continue
		}
		handoffs++
		if fact.Handoff.MessageID == overflow.ID {
			t.Fatalf("one-byte-overflow message acquired a durable lead handoff: %+v", fact)
		}
	}
	if handoffs != len(messageIDs)-1 {
		t.Fatalf("parent history contains %d handoffs, want exactly %d for accepted messages", handoffs, len(messageIDs)-1)
	}
}
