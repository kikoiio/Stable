package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"stable/internal/memory"
	"stable/internal/permission"
	"stable/internal/sessionlog"
)

type fakeMemoryProvider struct {
	headers []memory.MemoryHeader
	entry   memory.MemoryEntry
	err     error
	change  memory.MemoryChange
	deleted string
	session string
}

func (p *fakeMemoryProvider) List(_ context.Context, session string) ([]memory.MemoryHeader, error) {
	p.session = session
	return p.headers, p.err
}
func (p *fakeMemoryProvider) Read(_ context.Context, session string, _ memory.MemoryScope, _ string) (memory.MemoryEntry, error) {
	p.session = session
	return p.entry, p.err
}
func (p *fakeMemoryProvider) Save(_ context.Context, session string, change memory.MemoryChange) error {
	p.session, p.change = session, change
	return p.err
}
func (p *fakeMemoryProvider) Delete(_ context.Context, session string, _ memory.MemoryScope, filename string) error {
	p.session, p.deleted = session, filename
	return p.err
}

func TestMemoryToolSchemasHaveNoPathInputs(t *testing.T) {
	schemas := MemoryToolSchemas()
	if len(schemas) != 4 {
		t.Fatalf("schema count = %d, want 4", len(schemas))
	}
	for _, schema := range schemas {
		input := schemaObject(t, schema, "input_schema")
		properties := schemaObject(t, input, "properties")
		for _, key := range []string{"path", "root", "project_root", "user_root", "directory", "file_path"} {
			if _, exists := properties[key]; exists {
				t.Fatalf("%s schema accepts path field %q", schema["name"], key)
			}
		}
	}
}

func TestMemorySaveHostToolScopesAndOmitsBodyFromAudit(t *testing.T) {
	formal, sessionRoot := t.TempDir(), t.TempDir()
	info, err := sessionlog.Create(sessionRoot, "memory-tool")
	if err != nil {
		t.Fatal(err)
	}
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	authority.SessionID = info.ID
	provider := &fakeMemoryProvider{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Memory: provider, SessionRoot: sessionRoot, Now: time.Now})
	runner, err := factory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	secretBody := "preference body that must not be persisted in audit"
	call := m06Call("memory_save", `{"scope":"user","type":"feedback","name":"tone","description":"writing preference","body":"`+secretBody+`"}`)
	outcome, err := runner.Execute(context.Background(), call)
	requireOutcome(t, outcome, err, false, "Memory saved.")
	if provider.session != info.ID || provider.change.Action != memory.ActionUpsert || provider.change.Scope != memory.ScopeUser || provider.change.Type != memory.TypeFeedback || provider.change.Body != secretBody {
		t.Fatalf("provider received wrong save: %#v", provider)
	}
	replay, err := sessionlog.Replay(sessionRoot, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var gotAction bool
	for _, event := range replay.Events {
		encoded, _ := json.Marshal(event.Data)
		if strings.Contains(string(encoded), secretBody) {
			t.Fatalf("memory body leaked into %s: %s", event.Type, encoded)
		}
		if event.Type == sessionlog.EventMemoryAction {
			var action sessionlog.MemoryActionRecord
			_ = json.Unmarshal(encoded, &action)
			gotAction = action.Operation == "save" && action.Scope == "user" && action.State == "success"
		}
	}
	if !gotAction {
		t.Fatal("expected metadata-only successful memory_action event")
	}
}

func TestMemoryReadBodyIsNotLogged(t *testing.T) {
	formal, sessionRoot := t.TempDir(), t.TempDir()
	info, err := sessionlog.Create(sessionRoot, "memory-read")
	if err != nil {
		t.Fatal(err)
	}
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	authority.SessionID = info.ID
	provider := &fakeMemoryProvider{entry: memory.MemoryEntry{MemoryHeader: memory.MemoryHeader{Name: "hint", Type: memory.TypeUser}, Body: "private body payload"}}
	runner, err := NewToolExecutorFactory(ToolExecutorDeps{Memory: provider, SessionRoot: sessionRoot, Now: time.Now}).ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("memory_read", `{"scope":"user","filename":"hint.md"}`))
	requireOutcome(t, outcome, err, false, "private body payload")
	replay, err := sessionlog.Replay(sessionRoot, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range replay.Events {
		encoded, _ := json.Marshal(event.Data)
		if strings.Contains(string(encoded), "private body payload") {
			t.Fatalf("memory read body leaked into %s: %s", event.Type, encoded)
		}
	}
}

func TestMemorySaveRejectsScopeTypeMismatch(t *testing.T) {
	provider := &fakeMemoryProvider{}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{Memory: provider})
	outcome, err := runner.Execute(context.Background(), m06Call("memory_save", `{"scope":"project","type":"user","name":"x","description":"y","body":"z"}`))
	requireOutcome(t, outcome, err, true, "type does not belong")
	if provider.change.Body != "" {
		t.Fatal("provider must not be called for a scope/type mismatch")
	}
}

func TestMemoryProviderFailureIsReturned(t *testing.T) {
	provider := &fakeMemoryProvider{err: errors.New("memory unavailable")}
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{Memory: provider})
	outcome, err := runner.Execute(context.Background(), m06Call("memory_list", `{}`))
	requireOutcome(t, outcome, err, true, "memory unavailable")
}
