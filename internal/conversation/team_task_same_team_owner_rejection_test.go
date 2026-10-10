package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberCannotUpdateAnotherMembersTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "same-team-task-owner-lead")
	team, err := service.CreateTeam(context.Background(), leadRequest, "same-team-task-owner")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, leadRequest, team.ID, "member-a", "reader-a")
	addTeamMessageMember(t, service, leadRequest, team.ID, "member-b", "reader-b")
	task, err := service.CreateTeamTask(context.Background(), leadRequest, team.ID, teams.Task{Title: "B owned task", Assignee: "member-b"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Assignee != "member-b" || task.Status == teams.TaskCompleted {
		t.Fatalf("fixture task=%+v, want an unfinished task assigned to member B", task)
	}

	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	childRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{
		ID: turnID, MemberID: "member-a", RunID: childRunID, TaskID: turnID,
		OriginRunID: leadRequest.RunID, Status: "intent",
	}
	appendTeamOwnerRejectionTurnFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamOwnerRejectionTurnFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: childRunID, WorkKind: string(leadRequest.Work.Kind), Intent: "update another member task",
		TeamID: team.ID, TeamMemberID: "member-a", TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}
	memberRequest := leadRequest
	memberRequest.RunID = childRunID
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: "member-a", TurnID: turnID, MemberName: "reader-a"}

	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)
	if _, err := service.UpdateTeamTask(context.Background(), memberRequest, team.ID, task.ID, task.Revision, teams.TaskPatch{Title: stringPtr("member A unauthorized update")}); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("member A updating member B task error=%v, want permission error", err)
	}

	after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected same-team ownership update changed replay projection: before=%+v after=%+v", before, after)
	}
	got, err := service.GetTeamTask(context.Background(), leadRequest, team.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "B owned task" || got.Assignee != "member-b" || got.Revision != task.Revision || got.Status == teams.TaskCompleted {
		t.Fatalf("rejected update changed task owner/content/revision: got=%+v want initial=%+v", got, task)
	}
	transcript, err = sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount {
		t.Fatalf("rejected same-team ownership update appended facts: before=%d after=%d", eventCount, len(transcript.Events))
	}
}

func appendTeamOwnerRejectionTurnFact(t *testing.T, service *Service, request agent.ExecutionRequest, teamID, kind string, turn sessionlog.TurnFact) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(service.deps.ProjectRoot, request.Work.SessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(service.deps.ProjectRoot, request.Work.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: eventID, TeamID: teamID, SessionID: request.Work.SessionID, Kind: kind,
		Revision: projection.Teams[teamID].Revision + 1, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
}
