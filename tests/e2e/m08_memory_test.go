package e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/memory"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type m08Selector struct{ refs []memory.MemoryRef }

func (s m08Selector) Select(context.Context, string, []memory.MemoryHeader) ([]memory.MemoryRef, error) {
	return append([]memory.MemoryRef(nil), s.refs...), nil
}

type m08Processor struct{ changes []memory.MemoryChange }

func (p *m08Processor) Extract(context.Context, memory.ExtractionInput) ([]memory.MemoryChange, error) {
	return append([]memory.MemoryChange(nil), p.changes...), nil
}
func (*m08Processor) Consolidate(context.Context, memory.ConsolidationInput) ([]memory.MemoryChange, error) {
	return nil, nil
}

func TestM08MemoryLayersRecallExtractionAndManagement(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	configHome := filepath.Join(home, ".config")
	project := filepath.Join(root, "project")
	work := filepath.Join(project, "nested", "work")
	for _, dir := range []string{configHome, work, filepath.Join(project, ".stable")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	userMemoryDir, err := appconfig.UserMemoryDir()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(userMemoryDir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if writeErr := os.WriteFile(path, []byte(content), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	write(filepath.Join(configHome, "stable", "STABLE.md"), "User rule.\n")
	write(filepath.Join(project, "STABLE.md"), "Project rule.\n@./include.md\n")
	write(filepath.Join(project, "include.md"), "Included project rule.\n")
	write(filepath.Join(work, "STABLE.local.md"), "Work rule.\n")
	goalFacts := filepath.Join(project, "goal-facts.json")
	write(goalFacts, `{"criterion":"unchanged"}`)

	storeRoot, err := memory.NewStore(project, configHome)
	if err != nil {
		t.Fatal(err)
	}
	userHeader, err := storeRoot.Save(memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeUser, Name: "Tone", Description: "Concise answers", Body: "Use short direct replies."})
	if err != nil {
		t.Fatal(err)
	}
	projectHeader, err := storeRoot.Save(memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeProject, Type: memory.TypeReference, Name: "Build", Description: "Build command", Body: "Run go test ./... ."})
	if err != nil {
		t.Fatal(err)
	}
	processor := &m08Processor{changes: []memory.MemoryChange{{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeFeedback, Name: "Language", Description: "User language", Body: "Prefer Chinese explanations."}}}
	manager, err := memory.NewManager(memory.Options{
		ProjectRoot: project, UserConfigDir: configHome, StateDir: filepath.Join(root, "state"), Processor: processor,
		Selector: m08Selector{refs: []memory.MemoryRef{{Scope: memory.ScopeUser, Filename: userHeader.Filename}, {Scope: memory.ScopeProject, Filename: projectHeader.Filename}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	ctx := context.Background()
	prepared, err := manager.PrepareRun(ctx, project, work, "session-memory", "answer in Chinese and inspect build")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"User rule.", "Project rule.", "Included project rule.", "Work rule."} {
		if !strings.Contains(prepared.InstructionText, want) {
			t.Fatalf("missing layered instruction %q in %q", want, prepared.InstructionText)
		}
	}
	if len(prepared.Selected) != 2 || prepared.Selected[0].Body == "" || prepared.Selected[1].Body == "" || !strings.Contains(prepared.UserIndex, "Tone") || !strings.Contains(prepared.ProjectIndex, "Build") {
		t.Fatalf("memory recall/index context = %+v", prepared)
	}
	manager.CompleteRun(ctx, memory.RunCompletion{ProjectRoot: project, SessionID: "session-memory", RunID: "run-extract", WorkKind: "session", Messages: []memory.ConversationText{{Seq: 1, Kind: "session_text", Text: "Please use Chinese explanations."}}, ThroughSeq: 4})

	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(project, "memory e2e")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	socket := filepath.Join(root, "conversation.sock")
	svcCtx, cancel := context.WithCancel(ctx)
	gate := conversation.NewMemoryGate(manager, project)
	svc, err := conversation.Serve(svcCtx, conversation.Deps{Store: db, ProjectRoot: project, SocketPath: socket, Memory: gate})
	if err != nil {
		cancel()
		db.Close()
		t.Fatal(err)
	}
	defer func() { cancel(); svc.Close(); db.Close() }()
	waitMemorySocket(t, socket)
	list, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "memory_list", SessionID: session.ID})
	if err != nil || len(list) != 1 || len(list[0].MemoryEntries) < 2 {
		t.Fatalf("memory list response = %#v, %v", list, err)
	}
	clear, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "memory_clear", SessionID: session.ID})
	if err != nil || len(clear) != 1 || clear[0].MemoryReport == nil || clear[0].MemoryReport.Scope != "project" {
		t.Fatalf("default memory clear response = %#v, %v", clear, err)
	}
	remaining, err := manager.List(ctx, project)
	if err != nil || len(remaining) < 1 || remaining[0].Scope != memory.ScopeUser {
		t.Fatalf("default clear changed user memories: %#v, %v", remaining, err)
	}
	closeCtx, closeCancel := context.WithTimeout(ctx, time.Second)
	defer closeCancel()
	if err = manager.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if err = waitExtracted(ctx, manager, project); err != nil {
		t.Fatal(err)
	}
	nextRun, err := manager.PrepareRun(ctx, project, work, "session-next", "explain this project")
	if err != nil || !strings.Contains(nextRun.UserIndex, "Language") {
		t.Fatalf("extracted memory was not available on the next run: %+v, %v", nextRun, err)
	}
	if got, readErr := os.ReadFile(goalFacts); readErr != nil || string(got) != `{"criterion":"unchanged"}` {
		t.Fatalf("goal facts changed: %q, %v", got, readErr)
	}
}

func waitMemorySocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("memory conversation socket did not become ready")
}

func waitExtracted(ctx context.Context, manager memory.Manager, project string) error {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		entries, err := manager.List(ctx, project)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name == "Language" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return os.ErrDeadlineExceeded
		case <-ticker.C:
		}
	}
}
