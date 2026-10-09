package tui

import (
	"context"
	"os"
	"path/filepath"
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
	model, createAResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create prerequisite A")
	createA := acceptanceTeamResponse(t, createAResult, "team_task_create")
	model, createBResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create dependent B")
	createB := acceptanceTeamResponse(t, createBResult, "team_task_create")
	if createA.TeamTask == nil || createB.TeamTask == nil {
		t.Fatalf("TUI task creation responses A=%+v B=%+v", createA, createB)
	}
	dependency := []string{createA.TeamTask.ID}
	dependencyResponse, err := conversation.Request(reqctx, socket, conversation.ClientMsg{
		Op: "team_task_update", SessionID: sessionID, TeamID: teamID, TaskID: createB.TeamTask.ID,
		ExpectedRevision: createB.TeamTask.Revision, TaskBlockedBy: &dependency,
	})
	if err != nil || len(dependencyResponse) != 1 || dependencyResponse[0].TeamTask == nil || len(dependencyResponse[0].TeamTask.BlockedBy) != 1 || dependencyResponse[0].TeamTask.BlockedBy[0] != createA.TeamTask.ID {
		t.Fatalf("persist B dependency: response=%+v err=%v", dependencyResponse, err)
	}
	model, listBlocked := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	_ = model
	blockedTasks := acceptanceTeamResponse(t, listBlocked, "team_task_list").TeamTasks
	blockedB := findTeamTask(blockedTasks, createB.TeamTask.ID)
	blockedA := findTeamTask(blockedTasks, createA.TeamTask.ID)
	if blockedB.Status != teams.TaskBlocked || len(blockedB.BlockedBy) != 1 || blockedB.BlockedBy[0] != blockedA.ID || len(blockedA.Blocks) != 1 || blockedA.Blocks[0] != blockedB.ID {
		t.Fatalf("TUI dependency projection B=%+v A=%+v", blockedB, blockedA)
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
	if _, err := svc.UpdateTeamTask(ctx, requests[1], teamID, createB.TeamTask.ID, createB.TeamTask.Revision+1, teams.TaskPatch{Assignee: &assigneeB, Status: &statusProgress}); err == nil {
		t.Fatal("member B claim before prerequisite completion succeeded")
	}
	beforeComplete, err := svc.GetTeamTask(ctx, agent.ExecutionRequest{Work: requests[1].Work, TeamUser: true}, teamID, createB.TeamTask.ID)
	if err != nil || beforeComplete.Assignee != "" || beforeComplete.Status != teams.TaskBlocked || beforeComplete.Revision != createB.TeamTask.Revision+1 {
		t.Fatalf("blocked member claim mutated B: task=%+v err=%v", beforeComplete, err)
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
	if readyB.Status != teams.TaskPending || len(findTeamTask(readyTasks, createA.TeamTask.ID).Blocks) != 1 {
		t.Fatalf("B did not unblock after A completion: %+v", readyTasks)
	}
	assigneeB = memberIDs[1]
	claimedB, err := svc.UpdateTeamTask(ctx, requests[1], teamID, readyB.ID, readyB.Revision, teams.TaskPatch{Assignee: &assigneeB, Status: &statusProgress})
	if err != nil || claimedB.Assignee != assigneeB || claimedB.Status != teams.TaskInProgress {
		t.Fatalf("member B claim after unblock=%+v err=%v", claimedB, err)
	}
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
