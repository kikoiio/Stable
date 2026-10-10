package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamTaskDependencyFlowsFromTUIThroughMemberClaimsAndReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	shortSocketDir, err := filepath.Abs(filepath.Join("..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shortSocketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp(shortSocketDir, "task-")
	if err != nil {
		t.Fatal(err)
	}
	socket := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	svc, err := conversation.Serve(ctx, conversation.Deps{Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	todo := sessionlog.TodoUpdate{Revision: 1, Tasks: []sessionlog.TaskSnapshot{{ID: "m06-session-task", Subject: "separate session todo", Status: "pending"}}}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTodo, todo); err != nil {
		t.Fatal(err)
	}
	teamID := mustTeamID(t)
	appendTeamFact := func(kind, actor string, revision uint64, team *teams.Team, member *teams.Member, turn *sessionlog.TurnFact) {
		t.Helper()
		id, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		if _, appendErr := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
			ID: id, TeamID: teamID, SessionID: sessionID, Kind: kind, Revision: revision,
			ActorID: actor, Team: team, Member: member, Turn: turn,
		}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	team := teams.Team{ID: teamID, Name: "task-dependency", Scope: teams.Scope{SessionID: sessionID, WorkKind: "session", ProjectRoot: project}, Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC()}
	appendTeamFact(sessionlog.TeamCreated, teams.Lead, 1, &team, nil, nil)
	memberIDs := []string{mustTeamID(t), mustTeamID(t)}
	for i, memberID := range memberIDs {
		member := teams.Member{ID: memberID, TeamID: teamID, Name: []string{"worker-a", "worker-b"}[i], AgentName: "explore", RoleHash: "fixture", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
		appendTeamFact(sessionlog.TeamMemberAdded, teams.Lead, uint64(2+i), nil, &member, nil)
	}

	model := New(socket, project)
	model.ActiveSession = sessionID
	model, createAResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create prerequisite A --assignee "+memberIDs[0]+" --description prepare the shared prerequisite")
	createA := acceptanceTeamResponse(t, createAResult, "team_task_create")
	if createA.TeamTask == nil {
		t.Fatalf("TUI task creation response A=%+v", createA)
	}
	model, createBResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create dependent B")
	createB := acceptanceTeamResponse(t, createBResult, "team_task_create")
	if createB.TeamTask == nil {
		t.Fatalf("TUI task creation responses A=%+v B=%+v", createA, createB)
	}
	model, updateBResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks update "+createB.TeamTask.ID+" "+strconv.FormatUint(createB.TeamTask.Revision, 10)+" assignee "+memberIDs[1]+" blocked_by "+createA.TeamTask.ID)
	updateB := acceptanceTeamResponse(t, updateBResult, "team_task_update")
	if updateB.TeamTask == nil {
		t.Fatalf("TUI task update response B=%+v", updateB)
	}
	createB.TeamTask = updateB.TeamTask
	if createA.TeamTask.Assignee != memberIDs[0] || createA.TeamTask.Description != "prepare the shared prerequisite" || createB.TeamTask.Assignee != memberIDs[1] || len(createB.TeamTask.BlockedBy) != 1 || createB.TeamTask.BlockedBy[0] != createA.TeamTask.ID {
		t.Fatalf("TUI task create/update did not persist ownership/description/dependency: A=%+v B=%+v", createA.TeamTask, createB.TeamTask)
	}
	model, listBlocked := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	blockedTasks := acceptanceTeamResponse(t, listBlocked, "team_task_list").TeamTasks
	blockedB := findTeamTask(blockedTasks, createB.TeamTask.ID)
	blockedA := findTeamTask(blockedTasks, createA.TeamTask.ID)
	if blockedB.Status != teams.TaskBlocked || len(blockedB.BlockedBy) != 1 || blockedB.BlockedBy[0] != blockedA.ID || len(blockedA.Blocks) != 1 || blockedA.Blocks[0] != blockedB.ID {
		t.Fatalf("TUI dependency projection B=%+v A=%+v", blockedB, blockedA)
	}
	// Parse failures are returned to the TUI before any socket request is sent.
	invalidModel := model
	invalidModel.Composer.SetValue("/team " + teamID + " tasks update " + blockedB.ID + " 0 status completed")
	invalidUpdated, invalidCmd := invalidModel.submitComposer()
	if invalidCmd != nil || !strings.Contains(invalidUpdated.(Model).Status, "用法") {
		t.Fatalf("invalid task revision was not reported: command=%v status=%q", invalidCmd != nil, invalidUpdated.(Model).Status)
	}

	requests := make([]agent.ExecutionRequest, 2)
	for i, memberID := range memberIDs {
		runID, turnID, taskID := mustTeamID(t), mustTeamID(t), mustTeamID(t)
		turn := sessionlog.TurnFact{ID: turnID, MemberID: memberID, RunID: runID, TaskID: taskID, Status: "intent"}
		appendTeamFact(sessionlog.TeamTurnIntent, "service", 6+uint64(i*2)+1, nil, nil, &turn)
		turn.Status = "queued"
		appendTeamFact(sessionlog.TeamTurnAccepted, "service", 6+uint64(i*2)+2, nil, nil, &turn)
		if _, err := sessionlog.Append(project, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: runID, WorkKind: string(agent.WorkSession), TeamID: teamID, TeamMemberID: memberID, TeamTurnID: turnID, Intent: "task board member fixture",
		}); err != nil {
			t.Fatal(err)
		}
		requests[i] = agent.ExecutionRequest{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, TeamTurn: &agent.TeamTurnIdentity{TeamID: teamID, MemberID: memberID, TurnID: turnID}}
	}

	assigneeB, statusProgress := memberIDs[1], teams.TaskInProgress
	if _, err := svc.UpdateTeamTask(ctx, requests[1], teamID, createB.TeamTask.ID, createB.TeamTask.Revision, teams.TaskPatch{Assignee: &assigneeB, Status: &statusProgress}); err == nil {
		t.Fatal("member B claim before prerequisite completion succeeded")
	}
	model, listStillBlocked := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	stillBlocked := findTeamTask(acceptanceTeamResponse(t, listStillBlocked, "team_task_list").TeamTasks, createB.TeamTask.ID)
	if stillBlocked.Assignee != memberIDs[1] || stillBlocked.Status != teams.TaskBlocked || stillBlocked.Revision != createB.TeamTask.Revision {
		t.Fatalf("blocked member claim mutated B: task=%+v", stillBlocked)
	}

	assigneeA := memberIDs[0]
	claimedA, err := svc.UpdateTeamTask(ctx, requests[0], teamID, createA.TeamTask.ID, createA.TeamTask.Revision, teams.TaskPatch{Assignee: &assigneeA, Status: &statusProgress})
	if err != nil || claimedA.Assignee != assigneeA || claimedA.Status != teams.TaskInProgress {
		t.Fatalf("member A claim=%+v err=%v", claimedA, err)
	}
	statusCompleted := teams.TaskCompleted
	completedA, err := svc.UpdateTeamTask(ctx, requests[0], teamID, createA.TeamTask.ID, claimedA.Revision, teams.TaskPatch{Status: &statusCompleted})
	if err != nil || completedA.Status != teams.TaskCompleted {
		t.Fatalf("member A complete=%+v err=%v", completedA, err)
	}
	model, listReady := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	readyTasks := acceptanceTeamResponse(t, listReady, "team_task_list").TeamTasks
	readyB := findTeamTask(readyTasks, createB.TeamTask.ID)
	if readyB.Status != teams.TaskPending || readyB.Assignee != memberIDs[1] || len(findTeamTask(readyTasks, createA.TeamTask.ID).Blocks) != 1 {
		t.Fatalf("B did not unblock after A completion: %+v", readyTasks)
	}
	assigneeB = memberIDs[1]
	claimedB, err := svc.UpdateTeamTask(ctx, requests[1], teamID, readyB.ID, readyB.Revision, teams.TaskPatch{Assignee: &assigneeB, Status: &statusProgress})
	if err != nil || claimedB.Assignee != assigneeB || claimedB.Status != teams.TaskInProgress {
		t.Fatalf("member B claim after unblock=%+v err=%v", claimedB, err)
	}
	// A lead edit with the now stale pre-claim revision must surface the
	// service error instead of presenting an apparently successful mutation.
	beforeConflict, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	staleModel := model
	staleModel.Composer.SetValue("/team " + teamID + " tasks update " + claimedB.ID + " " + strconv.FormatUint(readyB.Revision, 10) + " blocked_by none")
	staleUpdated, staleCmd := staleModel.submitComposer()
	if staleCmd == nil {
		t.Fatalf("stale update did not reach the service: status=%q", staleUpdated.(Model).Status)
	}
	staleCommandResult := staleCmd()
	staleResponse, ok := staleCommandResult.(resultMsg)
	if !ok {
		t.Fatalf("stale update returned unexpected command result %T", staleCommandResult)
	}
	staleUpdated, _ = staleUpdated.(Model).handleResult(staleResponse)
	if staleResponse.err == nil || !strings.Contains(staleUpdated.(Model).Status, staleResponse.err.Error()) {
		t.Fatalf("stale revision feedback missing: status=%q err=%v", staleUpdated.(Model).Status, staleResponse.err)
	}
	staleModel = staleUpdated.(Model)
	refreshedTask := findTeamTask(staleModel.TeamTasks, claimedB.ID)
	projectionAfterConflict, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	durableTask := projectionAfterConflict.Tasks[claimedB.ID]
	if refreshedTask.ID == "" || refreshedTask.Revision != durableTask.Revision || durableTask.Revision != claimedB.Revision {
		t.Fatalf("stale conflict did not refresh the current task revision: TUI=%+v ReplayTeams=%+v claimed=%+v", refreshedTask, durableTask, claimedB)
	}
	visibleCurrentRevision := false
	for _, event := range staleModel.Events {
		if event.Type != sessionlog.EventMessage {
			continue
		}
		message, ok := event.Data.(sessionlog.Message)
		if ok && strings.Contains(message.Text, claimedB.ID) && strings.Contains(message.Text, "revision "+strconv.FormatUint(durableTask.Revision, 10)) {
			visibleCurrentRevision = true
			break
		}
	}
	if !visibleCurrentRevision {
		t.Fatalf("stale conflict did not show current revision %d in TUI events: %+v", durableTask.Revision, staleModel.Events)
	}
	afterConflict, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterConflict.Events) != len(beforeConflict.Events) {
		t.Fatalf("stale task conflict appended durable session events: before=%d after=%d", len(beforeConflict.Events), len(afterConflict.Events))
	}
	assertTeamDependencyTodoSnapshot(t, project, sessionID, todo)
	completedB, err := svc.UpdateTeamTask(ctx, requests[1], teamID, readyB.ID, claimedB.Revision, teams.TaskPatch{Status: &statusCompleted})
	if err != nil || completedB.Status != teams.TaskCompleted {
		t.Fatalf("member B complete=%+v err=%v", completedB, err)
	}
	model, finalList := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	finalTasks := acceptanceTeamResponse(t, finalList, "team_task_list").TeamTasks
	finalA, finalB := findTeamTask(finalTasks, createA.TeamTask.ID), findTeamTask(finalTasks, createB.TeamTask.ID)
	if finalA.Status != teams.TaskCompleted || finalB.Status != teams.TaskCompleted || len(finalA.Blocks) != 1 || finalA.Blocks[0] != finalB.ID {
		t.Fatalf("final TUI task board=%+v", finalTasks)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tasks[finalA.ID].Status != teams.TaskCompleted || projection.Tasks[finalB.ID].Status != teams.TaskCompleted || len(projection.Tasks[finalB.ID].BlockedBy) != 1 || projection.Tasks[finalB.ID].BlockedBy[0] != finalA.ID {
		t.Fatalf("final ReplayTeams task facts A=%+v B=%+v", projection.Tasks[finalA.ID], projection.Tasks[finalB.ID])
	}
	assertTeamDependencyTodoSnapshot(t, project, sessionID, todo)
}

func assertTeamDependencyTodoSnapshot(t *testing.T, root, sessionID string, expected sessionlog.TodoUpdate) {
	t.Helper()
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	updates := make([]sessionlog.TodoUpdate, 0, 1)
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTodo {
			continue
		}
		data, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		var update sessionlog.TodoUpdate
		if err := json.Unmarshal(data, &update); err != nil {
			t.Fatal(err)
		}
		updates = append(updates, update)
	}
	if len(updates) != 1 || !reflect.DeepEqual(updates[0], expected) {
		t.Fatalf("team task workflow mutated M06 todo: got=%+v want=%+v", updates, expected)
	}
}

func mustTeamID(t *testing.T) string {
	t.Helper()
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func findTeamTask(tasks []teams.Task, id string) teams.Task {
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	return teams.Task{}
}
