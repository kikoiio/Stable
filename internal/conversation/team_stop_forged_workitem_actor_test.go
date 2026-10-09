package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberStopRejectsForgedWorkItemActorBeforeDispatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "workitem-stop-owner-run")
	team, err := service.CreateTeam(t.Context(), request, "stop-actor-scope")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "member-stop-actor-scope"
	addTeamMessageMember(t, service, request, team.ID, memberID, "reader")
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	turn := sessionlog.TurnFact{
		ID: "turn-stop-actor-scope", MemberID: member.ID, RunID: "child-stop-actor-scope",
		TaskID: "task-stop-actor-scope", OriginRunID: request.RunID, OriginCallID: "spawn-stop-actor-scope", Status: "intent",
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, turn.RunID, turn.ID
	member.Budget.AcceptedTurns++
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", Member: &member}); err != nil {
		t.Fatal(err)
	}

	cancelCalls := 0
	service.teamScheduler = newTeamScheduler(service)
	service.teamScheduler.active[turn.ID] = func() { cancelCalls++ }
	service.teamScheduler.activeMember[turn.ID] = member.ID
	service.teamScheduler.activeTeam[turn.ID] = team.ID
	service.teamScheduler.activeOrigin[turn.ID] = request.RunID
	t.Cleanup(service.teamScheduler.close)

	historyBefore, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	transcriptBefore, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// A sibling WorkItem's active run cannot act as the local UI user who owns
	// the session-scoped stop operation. Protocol validation must reject it
	// before handleTeamRequest can mutate the target member or cancel its turn.
	forged, err := json.Marshal(ClientMsg{
		Op: "team_member_stop", SessionID: request.Work.SessionID, TeamID: team.ID, TeamMemberID: member.ID,
		RunID: "sibling-workitem-run", GoalID: "same-goal", WorkItemID: "sibling-item",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodeErr := decodeClient(strings.NewReader(string(forged)))
	if decodeErr == nil {
		_, decodeErr = service.handleTeamRequest(t.Context(), decoded)
	}
	if decodeErr == nil {
		t.Fatal("forged sibling WorkItem actor was accepted for team_member_stop")
	}
	if cancelCalls != 0 {
		t.Fatalf("rejected sibling WorkItem stop canceled child %d times", cancelCalls)
	}
	historyAfter, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	transcriptAfter, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historyAfter, historyBefore) || !reflect.DeepEqual(transcriptAfter.Events, transcriptBefore.Events) {
		t.Fatalf("rejected sibling WorkItem stop changed facts: history=%d/%d session events=%d/%d", len(historyBefore), len(historyAfter), len(transcriptBefore.Events), len(transcriptAfter.Events))
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID]; got.Status != teams.MemberQueued || got.TurnID != turn.ID {
		t.Fatalf("rejected sibling WorkItem stop changed member: %+v", got)
	}
}
