package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// This exercises task mutations through the user-facing slash command, the
// conversation Unix socket and service, then checks the durable projections.
func TestTeamTaskBoardTUICreateUpdateListPersistsOwnerRevisionAndKeepsSessionTodo(t *testing.T) {
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
	// Keep the Unix socket below the platform's short AF_UNIX path limit. The
	// project-local TMPDIR path is longer than that limit on this machine.
	socketDir, err := os.MkdirTemp("/tmp", "m09-tt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
	})
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
	teamID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	team := teams.Team{
		ID: teamID, Name: "task-board",
		Scope: teams.Scope{SessionID: sessionID, WorkKind: "session", ProjectRoot: project},
		Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: eventID, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamCreated,
		Revision: 1, ActorID: teams.Lead, Team: &team,
	}); err != nil {
		t.Fatal(err)
	}
	todo := sessionlog.TodoUpdate{Revision: 1, Tasks: []sessionlog.TaskSnapshot{{ID: "m06-todo", Subject: "M06 session todo", Status: "pending"}}}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTodo, todo); err != nil {
		t.Fatal(err)
	}

	model := New(socket, project)
	model.ActiveSession = sessionID
	model, createResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create inspect parser")
	createdResponse := acceptanceTeamResponse(t, createResult, "team_task_create")
	if createdResponse.TeamTask == nil || createdResponse.TeamTask.Title != "inspect parser" || createdResponse.TeamTask.Revision != 1 || createdResponse.TeamTask.CreatedBy != teams.Lead {
		t.Fatalf("TUI task creation=%+v, want lead-owned revision 1 task", createdResponse.TeamTask)
	}
	task := createdResponse.TeamTask

	model, updateResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks update "+task.ID+" 1 status in_progress")
	updatedResponse := acceptanceTeamResponse(t, updateResult, "team_task_update")
	if updatedResponse.TeamTask == nil || updatedResponse.TeamTask.ID != task.ID || updatedResponse.TeamTask.Status != teams.TaskInProgress || updatedResponse.TeamTask.Revision != 2 || updatedResponse.TeamTask.CreatedBy != teams.Lead {
		t.Fatalf("TUI task update=%+v, want lead-owned in-progress revision 2 task", updatedResponse.TeamTask)
	}

	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	listedResponse := acceptanceTeamResponse(t, listResult, "team_task_list")
	if len(listedResponse.TeamTasks) != 1 || listedResponse.TeamTasks[0].ID != task.ID || listedResponse.TeamTasks[0].Revision != 2 || listedResponse.TeamTasks[0].CreatedBy != teams.Lead || listedResponse.TeamTasks[0].Status != teams.TaskInProgress {
		t.Fatalf("TUI task list=%+v, want durable owner/revision/status", listedResponse.TeamTasks)
	}

	projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	replayed := projection.Tasks[task.ID]
	if replayed.ID != task.ID || replayed.Revision != 2 || replayed.CreatedBy != teams.Lead || replayed.Status != teams.TaskInProgress {
		t.Fatalf("ReplayTeams task=%+v, want updated durable task", replayed)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var todoFacts []sessionlog.TodoUpdate
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventTodo {
			continue
		}
		var update sessionlog.TodoUpdate
		raw, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &update); err != nil {
			t.Fatal(err)
		}
		todoFacts = append(todoFacts, update)
	}
	if len(todoFacts) != 1 || todoFacts[0].Revision != todo.Revision || len(todoFacts[0].Tasks) != 1 || todoFacts[0].Tasks[0].ID != "m06-todo" {
		t.Fatalf("team task commands mutated the independent M06 todo snapshot: %+v", todoFacts)
	}
}
