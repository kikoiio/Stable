package conversation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"stable/internal/memory"
	"stable/internal/sessionlog"
)

func TestMemoryGateBindsRootsAndClearDefaultsToProject(t *testing.T) {
	project, user, state := t.TempDir(), t.TempDir(), t.TempDir()
	manager, err := memory.NewManager(memory.Options{ProjectRoot: project, UserConfigDir: user, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if err := manager.Save(context.Background(), project, memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeProject, Type: memory.TypeProject, Name: "Project", Body: "project context"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(context.Background(), project, memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeUser, Name: "Preference", Body: "user preference"}); err != nil {
		t.Fatal(err)
	}
	info, err := sessionlog.Create(project, "memory-command")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{ProjectRoot: project}}
	gate := NewMemoryGate(manager, project)
	gate.Bind(svc)
	responses, err := svc.clearMemory(context.Background(), info.ID, "")
	if err != nil || responses[0].MemoryReport == nil || responses[0].MemoryReport.Scope != "project" || responses[0].MemoryReport.Deleted != 1 {
		t.Fatalf("default clear response = %#v, %v", responses, err)
	}
	entries, err := gate.List(context.Background(), info.ID)
	if err != nil || len(entries) != 1 || entries[0].Scope != memory.ScopeUser {
		t.Fatalf("default project clear affected user memories: %#v, %v", entries, err)
	}
	if _, err = svc.clearMemory(context.Background(), info.ID, "all"); err != nil {
		t.Fatal(err)
	}
	entries, err = gate.List(context.Background(), info.ID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("explicit all clear left entries: %#v, %v", entries, err)
	}
	replay, err := sessionlog.Replay(project, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var clearScopes []string
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventMemoryAction {
			continue
		}
		var record sessionlog.MemoryActionRecord
		data, _ := json.Marshal(event.Data)
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.Operation == "clear" {
			clearScopes = append(clearScopes, record.Scope)
		}
	}
	if len(clearScopes) != 2 || clearScopes[0] != "project" || clearScopes[1] != "all" {
		t.Fatalf("clear audit scopes = %v", clearScopes)
	}
}

func TestAppendMemoryBackgroundEventWritesMetadataOnly(t *testing.T) {
	root := t.TempDir()
	info, err := sessionlog.Create(root, "memory-worker")
	if err != nil {
		t.Fatal(err)
	}
	AppendMemoryBackgroundEvent(root, memory.BackgroundEvent{Action: "extract", State: "success", Count: 2, SessionID: info.ID, At: time.Now().UTC()})
	replay, err := sessionlog.Replay(root, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Events) != 2 || replay.Events[1].Type != sessionlog.EventMemoryBackground {
		t.Fatalf("background events = %+v", replay.Events)
	}
	var record sessionlog.MemoryBackgroundRecord
	data, _ := json.Marshal(replay.Events[1].Data)
	if err := json.Unmarshal(data, &record); err != nil || record.Action != "extract" || record.State != "success" || record.Count != 2 {
		t.Fatalf("background event record = %+v, %v", record, err)
	}
}
