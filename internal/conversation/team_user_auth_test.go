package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestForgedTeamUserCannotSendButLocalSocketCan(t *testing.T) {
	service, lead, team := newTeamMessageFixture(t)
	scheduler := &teamScheduler{
		readySet:        map[string]bool{},
		readyGeneration: map[string]uint64{},
		wake:            make(chan struct{}, 1),
	}
	service.teamScheduler = scheduler

	beforeProjection, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, lead.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(service.deps.ProjectRoot, lead.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(service.deps.ProjectRoot, lead.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	forged := lead
	forged.RunID = "forged-team-user-run"
	forged.TeamUser = true
	message, err := service.SendTeamMessage(t.Context(), forged, TeamSendRequest{
		TeamID: team.ID, Recipient: "member-a", Body: "forged local send", Token: "forged-user-token",
	})
	if !errors.Is(err, teams.ErrPermission) || message.ID != "" {
		t.Fatalf("forged TeamUser SendTeamMessage = %+v, %v; want empty message and ErrPermission", message, err)
	}
	afterProjection, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, lead.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(service.deps.ProjectRoot, lead.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessionlog.Replay(service.deps.ProjectRoot, lead.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistory, beforeHistory) || len(afterSession.Events) != len(beforeSession.Events) {
		t.Fatalf("forged TeamUser changed durable facts: projection=%v history=%d/%d session events=%d/%d", reflect.DeepEqual(afterProjection, beforeProjection), len(afterHistory), len(beforeHistory), len(afterSession.Events), len(beforeSession.Events))
	}
	scheduler.mu.Lock()
	readyBefore := append([]string(nil), scheduler.ready...)
	activeBefore := len(scheduler.active)
	scheduler.mu.Unlock()
	if len(readyBefore) != 0 || activeBefore != 0 {
		t.Fatalf("forged TeamUser changed scheduler before check: ready=%v active=%d", readyBefore, activeBefore)
	}

	response, err := service.handleTeamRequest(t.Context(), ClientMsg{
		Op: "team_send", SessionID: lead.Work.SessionID, TeamID: team.ID,
		TeamRecipient: "member-a", TeamToken: "local-user-token", Text: "authorized local send",
	})
	if err != nil || response.TeamMessage == nil || response.TeamMessage.Body != "authorized local send" {
		t.Fatalf("local socket user send = %+v, %v", response, err)
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if !reflect.DeepEqual(scheduler.ready, []string{"member-a"}) {
		t.Fatalf("authorized local send did not schedule its recipient: ready=%v", scheduler.ready)
	}
}

func TestTeamUserProofBindsWorkAndIsNotSerialized(t *testing.T) {
	service, lead, team := newTeamMessageFixture(t)
	request, err := service.teamUserRequest(context.Background(), lead.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !request.TeamUser || request.TeamUserProof == "" {
		t.Fatalf("local request lacks user proof: %+v", request)
	}
	changedWork := request
	changedWork.Work.SessionID = "another-session"
	if _, _, _, err := service.teamOperationScope(context.Background(), changedWork); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("proof accepted changed WorkRef: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), request.TeamUserProof) {
		t.Fatalf("private TeamUser proof was serialized: %s", encoded)
	}
}
