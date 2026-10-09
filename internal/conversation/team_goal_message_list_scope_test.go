package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamMessageListRejectsAnotherWorkItemScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team message list scope")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err := state.CreateGoal(context.Background(), coreGoal("goal-message-scope", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-message-scope", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-message-scope", WorkItemID: "item-b"}
	for runID, work := range map[string]agent.WorkRef{"run-item-a": workA, "run-item-b": workB} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal scope fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{"run-item-a": session.ID, "run-item-b": session.ID},
	}
	requestA := agent.ExecutionRequest{RunID: "run-item-a", Work: workA}
	requestB := agent.ExecutionRequest{RunID: "run-item-b", Work: workB}
	team, err := service.CreateTeam(t.Context(), requestA, "item-a-team")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, requestA, team.ID, "member-a", "reader")
	if _, err := service.SendTeamMessage(t.Context(), requestA, TeamSendRequest{TeamID: team.ID, Recipient: "member-a", Body: "private item-a message", Token: "item-a-message"}); err != nil {
		t.Fatal(err)
	}
	messages, err := service.ListTeamMessages(t.Context(), requestA, team.ID, 0, teams.MaxPageSize)
	if err != nil || len(messages) != 1 || messages[0].Body != "private item-a message" {
		t.Fatalf("authorized work item could not list its message: messages=%+v err=%v", messages, err)
	}
	if messages, err = service.ListTeamMessages(t.Context(), requestB, team.ID, 0, teams.MaxPageSize); err == nil || len(messages) != 0 {
		t.Fatalf("different valid WorkItem read Goal team messages: messages=%+v err=%v", messages, err)
	}
	forged := requestA
	forged.Work.WorkItemID = workB.WorkItemID
	if messages, err = service.ListTeamMessages(t.Context(), forged, team.ID, 0, teams.MaxPageSize); err == nil || len(messages) != 0 {
		t.Fatalf("forged WorkItem read Goal team messages: messages=%+v err=%v", messages, err)
	}
}
