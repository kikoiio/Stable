package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamTaskMutationsRejectOtherGoalAndWorkItemWithoutFacts(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project")
	goalOneRoot := filepath.Join(root, "goal-one")
	goalTwoRoot := filepath.Join(root, "goal-two")
	for _, dir := range []string{goalOneRoot, goalTwoRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "goal task scope rejection")
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
	for id, goalRoot := range map[string]string{"goal-one": goalOneRoot, "goal-two": goalTwoRoot} {
		if _, err := state.CreateGoal(ctx, coreGoal(id, goalRoot, session.ID)); err != nil {
			t.Fatal(err)
		}
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-one", WorkItemID: "item-a"}
	otherItem := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-one", WorkItemID: "item-b"}
	otherGoal := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-two", WorkItemID: "item-b"}
	requests := map[string]agent.ExecutionRequest{}
	for runID, work := range map[string]agent.WorkRef{
		"goal-task-scope-a":      workA,
		"goal-task-scope-item-b": otherItem,
		"goal-task-scope-goal-b": otherGoal,
	} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal task scope rejection",
		}); err != nil {
			t.Fatal(err)
		}
		requests[runID] = agent.ExecutionRequest{RunID: runID, Work: work}
	}
	requestA := requests["goal-task-scope-a"]
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, "goal-task-scope-item-b": session.ID, "goal-task-scope-goal-b": session.ID},
	}
	team, err := service.CreateTeam(ctx, requestA, "goal-a-team")
	if err != nil {
		t.Fatal(err)
	}
	task, err := service.CreateTeamTask(ctx, requestA, team.ID, teams.Task{Title: "authorized goal A task"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEventCount := len(transcript.Events)

	actors := []struct {
		name    string
		request agent.ExecutionRequest
	}{
		{name: "valid run for another work item", request: requests["goal-task-scope-item-b"]},
		{name: "valid run for another goal and root", request: requests["goal-task-scope-goal-b"]},
		{name: "forged work item on authorized run", request: func() agent.ExecutionRequest { r := requestA; r.Work.WorkItemID = otherItem.WorkItemID; return r }()},
	}
	for _, actor := range actors {
		t.Run(actor.name, func(t *testing.T) {
			if _, err := service.CreateTeamTask(ctx, actor.request, team.ID, teams.Task{Title: "unauthorized task"}); err == nil {
				t.Fatal("cross-scope actor created a task")
			}
			if _, err := service.UpdateTeamTask(ctx, actor.request, team.ID, task.ID, task.Revision, teams.TaskPatch{Title: stringPtr("unauthorized update")}); err == nil {
				t.Fatal("cross-scope actor updated a task")
			}
		})
	}

	after, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected cross-scope task mutations changed team projection: before=%+v after=%+v", before, after)
	}
	transcript, err = sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != beforeEventCount {
		t.Fatalf("rejected cross-scope task mutations appended facts: before=%d after=%d", beforeEventCount, len(transcript.Events))
	}
	if got := after.Tasks[task.ID]; got.Title != "authorized goal A task" {
		t.Fatalf("authorized task changed after rejected mutations: %+v", got)
	}
}
