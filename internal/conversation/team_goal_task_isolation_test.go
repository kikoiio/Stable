package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamTaskBoardKeepsGoalStatusAndSessionTodoIndependent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team task isolation")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	goal := coreGoal("goal-task-isolation", goalRoot, session.ID)
	goal.Status = core.GoalActive
	if _, err := state.CreateGoal(ctx, goal); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goal.ID, WorkItemID: "item-task-isolation"}
	const runID = "goal-task-isolation-run"
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal team task isolation",
	}); err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: runID, Work: work}
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{runID: session.ID},
	}
	team, err := service.CreateTeam(ctx, request, "goal-task-isolation")
	if err != nil {
		t.Fatal(err)
	}

	todo := sessionlog.TodoUpdate{Revision: 1, Tasks: []sessionlog.TaskSnapshot{{ID: "m06-goal-session-todo", Subject: "session todo", Status: "pending"}}}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventTodo, todo); err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTeamTask(ctx, request, team.ID, teams.Task{Title: "verify goal work item"})
	if err != nil {
		t.Fatal(err)
	}
	completed := teams.TaskCompleted
	if _, err := service.UpdateTeamTask(ctx, request, team.ID, task.ID, task.Revision, teams.TaskPatch{Status: &completed}); err != nil {
		t.Fatal(err)
	}

	goalSnapshot, err := state.GetGoalSnapshot(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if goalSnapshot.Goal.Status != core.GoalActive {
		t.Fatalf("completing a Goal/WorkItem team task changed Goal status to %q, want %q", goalSnapshot.Goal.Status, core.GoalActive)
	}

	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var todoFacts []sessionlog.TodoUpdate
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTodo {
			continue
		}
		var update sessionlog.TodoUpdate
		if err := decodeSessionData(event.Data, &update); err != nil {
			t.Fatal(err)
		}
		todoFacts = append(todoFacts, update)
	}
	if len(todoFacts) != 1 || !reflect.DeepEqual(todoFacts[0], todo) {
		t.Fatalf("Goal/WorkItem team task operations changed session todo snapshot: %+v", todoFacts)
	}
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed := projection.Tasks[task.ID]
	if replayed.ID != task.ID || replayed.Status != teams.TaskCompleted {
		t.Fatalf("completed Goal/WorkItem team task was not independently replayed: %+v", replayed)
	}
}
