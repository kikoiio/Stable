package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
)

func TestCoordinatorToolsStayBoundToSelectedTeam(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "coordinator-owner")
	selected, err := service.CreateTeam(context.Background(), request, "selected")
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.CreateTeam(context.Background(), request, "other")
	if err != nil {
		t.Fatal(err)
	}
	request.TeamCoordinator = true
	request.TeamCoordinatorTeamID = selected.ID
	call := func(id, name string, args map[string]any) (agent.ToolOutcome, error) {
		encoded, _ := json.Marshal(args)
		return service.ExecuteTeamTool(context.Background(), request, llm.ToolUse{ID: id, Name: name, Arguments: encoded})
	}

	listed, err := call("list-selected", "team_list", map[string]any{})
	if err != nil || listed.Status != agent.ToolSucceeded {
		t.Fatalf("bound team list=%+v err=%v", listed, err)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(listed.Content), &list); err != nil || len(list) != 1 || list[0].ID != selected.ID {
		t.Fatalf("coordinator team list=%s err=%v", listed.Content, err)
	}
	created, err := call("create-selected-task", "team_task_create", map[string]any{"team_id": selected.ID, "title": "selected team only"})
	if err != nil || created.Status != agent.ToolSucceeded {
		t.Fatalf("selected team task create=%+v err=%v", created, err)
	}
	before, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := call("read-other-team", "team_get", map[string]any{"team_id": other.ID})
	if err != nil || denied.Status != agent.ToolDenied || !denied.IsError {
		t.Fatalf("cross-team coordinator read=%+v err=%v", denied, err)
	}
	denied, err = call("create-other-task", "team_task_create", map[string]any{"team_id": other.ID, "title": "must not persist"})
	if err != nil || denied.Status != agent.ToolDenied || !denied.IsError {
		t.Fatalf("cross-team coordinator write=%+v err=%v", denied, err)
	}
	after, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events) {
		t.Fatalf("denied coordinator calls appended events: before=%d after=%d", len(before.Events), len(after.Events))
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tasks) != 0 {
		t.Fatalf("cross-team task was persisted: %+v", projection.Tasks)
	}
}

func TestCoordinatorSelectionRejectsUnknownTeamWithoutPersistingMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "coordinator-invalid-selection")
	before, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.handleTeamRequest(context.Background(), ClientMsg{
		Op: "team_coordinator", SessionID: request.Work.SessionID,
		CoordinatorOn: true, CoordinatorTeamID: "f123456789abcdef0123456789abcdef",
	})
	if err == nil {
		t.Fatal("coordinator selected a team outside the session")
	}
	mode, modeErr := teamCoordinatorModeForSession(root, request.Work.SessionID)
	if modeErr != nil || mode.Enabled || mode.TeamID != "" {
		t.Fatalf("invalid selection changed coordinator mode: mode=%+v err=%v", mode, modeErr)
	}
	after, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil || len(after.Events) != len(before.Events) {
		t.Fatalf("invalid selection appended a mode event: before=%d after=%d err=%v", len(before.Events), len(after.Events), err)
	}
}
