package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestGoalRunCanSelectAuthorizedTeamCoordinatorForThatWorkItem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "Goal coordinator fixture")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	goal := core.Goal{ID: "goal-coordinator", Objective: "coordinate a scoped work item", AllowedRoot: goalRoot, SourceSessionID: session.ID}
	if _, err := state.CreateGoal(t.Context(), goal); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goal.ID, WorkItemID: "item-coordinator"}
	const fixtureRunID = "goal-coordinator-fixture-run"
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: fixtureRunID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "create the authorized team",
	}); err != nil {
		t.Fatal(err)
	}
	permissionBounds, err := json.Marshal(permission.Authority{RunID: fixtureRunID, SessionID: session.ID, GoalID: work.GoalID, WorkItemID: work.WorkItemID, AllowedRoot: goalRoot})
	if err != nil {
		t.Fatal(err)
	}
	fixtureRequest := agent.ExecutionRequest{RunID: fixtureRunID, Work: work, PermissionBounds: permissionBounds}
	defaults := []llm.ToolSchema{{Name: "read_file"}, {Name: "command"}, {Name: "team_list"}, {Name: "team_send"}, {Name: "team_task_update"}}
	runner := &coordinatorRecordingRunner{defaults: defaults, started: make(chan *coordinatorRunCapture, 1)}
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state, ProviderName: "fixture", Model: "model-v1", Runner: runner, ToolSchemas: defaults},
		activeRuns: map[string]string{fixtureRunID: session.ID}, activeRequests: map[string]agent.ExecutionRequest{fixtureRunID: fixtureRequest},
	}
	team, err := service.CreateTeam(t.Context(), fixtureRequest, "goal-coordinator-team")
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 8)
	var captured *coordinatorRunCapture
	t.Cleanup(func() {
		if captured != nil {
			captured.finish(agent.RunCompleted)
		}
	})
	request := agent.ExecutionRequest{
		RunID: "goal-coordinator-run", Work: work, Intent: "coordinate only the assigned Goal team",
		Messages: []llm.Message{{Role: "user", Content: "Review the assigned work item."}},
	}
	wrongScopeRequest := request
	wrongScopeRequest.RunID = "goal-coordinator-wrong-item"
	wrongScopeRequest.Work.WorkItemID = "another-item"
	wrongScopeMessage := ClientMsg{Op: "run_start", SessionID: session.ID, CoordinatorTeamID: team.ID, Run: &wrongScopeRequest}
	if err := validateClient(wrongScopeMessage); err != nil {
		t.Fatalf("syntactically valid Goal coordinator request was rejected before authorization: %v", err)
	}
	if err := service.startRun(t.Context(), wrongScopeMessage, updates); err == nil || !strings.Contains(err.Error(), "Goal coordinator team is not authorized") {
		t.Fatalf("cross-WorkItem coordinator selection error = %v, want scope rejection", err)
	}
	if _, found, findErr := sessionlog.FindRunStart(root, session.ID, wrongScopeRequest.RunID); findErr != nil || found {
		t.Fatalf("rejected cross-WorkItem coordinator run persisted a start: found=%v err=%v", found, findErr)
	}
	select {
	case got := <-runner.started:
		t.Fatalf("rejected cross-WorkItem coordinator reached runner: %+v", got.request)
	default:
	}
	message := ClientMsg{Op: "run_start", SessionID: session.ID, CoordinatorTeamID: team.ID, Run: &request}
	if err := validateClient(message); err != nil {
		t.Fatalf("valid explicit Goal coordinator parameter was rejected: %v", err)
	}
	if err := service.startRun(t.Context(), message, updates); err != nil {
		t.Fatal(err)
	}
	captured = receiveCoordinatorRun(t, runner.started)
	if !captured.request.TeamCoordinator || captured.request.TeamCoordinatorTeamID != team.ID {
		t.Fatalf("Goal run lost trusted coordinator binding: coordinator=%v team=%q", captured.request.TeamCoordinator, captured.request.TeamCoordinatorTeamID)
	}
	wantSchemas := []string{"team_list", "team_send", "team_task_update"}
	if got := schemaNames(captured.request.ToolSchemas); !reflect.DeepEqual(got, wantSchemas) {
		t.Fatalf("Goal run schemas=%v, want %v", got, wantSchemas)
	}
	if got := schemaNames(captured.effectiveSchema); !reflect.DeepEqual(got, wantSchemas) {
		t.Fatalf("Goal runner effective schemas=%v, want %v", got, wantSchemas)
	}
	if len(captured.request.Messages) == 0 || captured.request.Messages[0].Role != "system" || !strings.Contains(captured.request.Messages[0].Content, team.ID) || !strings.Contains(captured.request.Messages[0].Content, "团队协调器模式") {
		t.Fatalf("Goal run lacks its bound coordinator instruction: %+v", captured.request.Messages)
	}
	if !reflect.DeepEqual(captured.request.AllowedScope, []string{goalRoot}) {
		t.Fatalf("Goal coordinator widened trusted work root: %v", captured.request.AllowedScope)
	}
}
