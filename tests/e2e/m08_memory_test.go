package e2e

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/llm"
	"stable/internal/memory"
	"stable/internal/sessioncontext"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type m08Selector struct{ refs []memory.MemoryRef }

func (s m08Selector) Select(context.Context, string, []memory.MemoryHeader) ([]memory.MemoryRef, error) {
	return append([]memory.MemoryRef(nil), s.refs...), nil
}

type m08Processor struct {
	changes     []memory.MemoryChange
	extractSeen chan memory.ExtractionInput
}

func (p *m08Processor) Extract(_ context.Context, input memory.ExtractionInput) ([]memory.MemoryChange, error) {
	if p.extractSeen != nil {
		p.extractSeen <- input
	}
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
	return waitMemoryEntry(ctx, manager, project, "Language")
}

func waitMemoryEntry(ctx context.Context, manager memory.Manager, project, name string) error {
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
			if entry.Name == name {
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

func TestM08GoalWorkItemMemoryIsolation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	userConfig := filepath.Join(t.TempDir(), "config")
	stateDir := filepath.Join(t.TempDir(), "state")
	workDir := filepath.Join(root, "nested", "work")
	for _, dir := range []string{filepath.Join(userConfig, "stable"), workDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(userConfig, "stable", "STABLE.md"): "USER_GUIDANCE_M08",
		filepath.Join(root, "STABLE.md"):                 "PROJECT_GUIDANCE_M08",
		filepath.Join(root, "nested", "AGENTS.md"):       "NESTED_GUIDANCE_M08",
		filepath.Join(workDir, "STABLE.local.md"):        "WORK_GUIDANCE_M08",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sessionlog.Create(root, "memory goal")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		goalID     = "goal-memory-e2e"
		objective  = "GOAL_FACT_OBJECTIVE_7f2"
		criterion  = "GOAL_FACT_CRITERION_91a"
		progress   = "GOAL_FACT_PROGRESS_31c"
		evidence   = "GOAL_FACT_EVIDENCE_d08"
		conclusion = "GOAL_FACT_CONCLUSION_640"
		preference = "Please keep future explanations concise."
		memoryBody = "Use concise direct answers."
	)
	goalBefore, err := db.CreateGoal(ctx, core.Goal{
		ID: goalID, Objective: objective, Criteria: []core.Criterion{{ID: "criterion-1", Kind: "manual", Payload: json.RawMessage(`{"marker":"` + criterion + `"}`)}},
		AllowedRoot: workDir, SourceSessionID: session.ID, AllowedCapabilities: []string{"read_file"}, Status: core.GoalActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	memoryStore, err := memory.NewStore(root, userConfig)
	if err != nil {
		t.Fatal(err)
	}
	header, err := memoryStore.Save(memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeUser, Name: "Tone", Description: "Answer style", Body: memoryBody})
	if err != nil {
		t.Fatal(err)
	}
	extractSeen := make(chan memory.ExtractionInput, 2)
	processor := &m08Processor{extractSeen: extractSeen, changes: []memory.MemoryChange{{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeFeedback, Name: "Language", Description: "User preference", Body: memoryBody}}}
	manager, err := memory.NewManager(memory.Options{
		ProjectRoot: root, UserConfigDir: userConfig, StateDir: stateDir, Processor: processor,
		Selector: m08Selector{refs: []memory.MemoryRef{{Scope: memory.ScopeUser, Filename: header.Filename}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())

	provider := &m06Agent{respond: func(_ int, _ llm.Request) []llm.Event { return m06TextRound("Goal work completed.") }}
	runner := agent.NewRunner(provider, agent.RunnerOptions{MaxRetries: -1})
	socket := filepath.Join(t.TempDir(), "goal-memory.sock")
	svcCtx, cancel := context.WithCancel(ctx)
	svc, err := conversation.Serve(svcCtx, conversation.Deps{
		Store: db, ChatProvider: provider, Runner: runner, ProviderName: "fixture", Model: "fixture",
		ProjectRoot: root, SocketPath: socket, Memory: conversation.NewMemoryGate(manager, root), PollEvery: 10 * time.Millisecond,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { _ = svc.Close(); cancel() }()
	waitMemorySocket(t, socket)

	// Exercise the ordinary session path through the real socket runner first.
	// The extractor must receive this run's user text and persist its allowed
	// preference before the same service starts the Goal work item below.
	ordinaryText := "Please remember that I prefer concise explanations."
	ordinary := agent.ExecutionRequest{
		RunID: "ordinary-memory-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID},
		Intent: ordinaryText, Messages: []llm.Message{{Role: "user", Content: ordinaryText}}, ProviderName: "fixture", Model: "fixture",
	}
	ordinaryStream, err := conversation.OpenRun(ctx, socket, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryCompleted := false
	for !ordinaryCompleted {
		message, receiveErr := ordinaryStream.Receive()
		if receiveErr != nil {
			_ = ordinaryStream.Close()
			t.Fatalf("receive ordinary run: %v", receiveErr)
		}
		if message.Type == "error" {
			_ = ordinaryStream.Close()
			t.Fatalf("ordinary run failed: %s", message.Error)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				_ = ordinaryStream.Close()
				t.Fatalf("ordinary run outcome = %+v", message.Outcome)
			}
			ordinaryCompleted = true
		}
	}
	if err = ordinaryStream.Close(); err != nil {
		t.Fatal(err)
	}
	if !m08RoundContains(provider.roundMessages(1), memoryBody) {
		t.Fatalf("ordinary WorkSession provider request did not receive recalled memory: %+v", provider.roundMessages(1))
	}
	select {
	case input := <-extractSeen:
		if input.WorkKind != "session" || len(input.Messages) != 1 || input.Messages[0].Text != ordinaryText || input.Messages[0].Kind != "session_text" {
			t.Fatalf("ordinary extraction input did not contain only the run's user text: %+v", input)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary run memory extraction did not run")
	}
	if err = waitExtracted(ctx, manager, root); err != nil {
		t.Fatalf("ordinary run memory extraction did not persist: %v", err)
	}

	// Persist a real /say message through the conversation protocol. The goal
	// runner receives objective/criteria/progress/evidence/conclusion in its
	// work request, while extraction is allowed to see only this user reply.
	processor.changes = []memory.MemoryChange{{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeFeedback, Name: "GoalReply", Description: "Goal user reply preference", Body: memoryBody}}
	if _, err = conversation.Request(ctx, socket, conversation.ClientMsg{Op: "say", Goal: goalID, SessionID: session.ID, ProjectRoot: root, Text: preference}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []sessionlog.Message{
		{Role: "user", Kind: "goal_request", Text: objective + " " + criterion},
		{Role: "assistant", Kind: "text", Text: progress + " " + evidence + " " + conclusion},
		{Role: "user", Kind: "tool_result", Text: evidence},
	} {
		if _, err = sessionlog.Append(root, session.ID, sessionlog.EventMessage, msg); err != nil {
			t.Fatal(err)
		}
	}

	request := agent.ExecutionRequest{
		RunID: "goal-memory-run", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: goalID, WorkItemID: "item-memory-1"},
		Intent:       strings.Join([]string{objective, criterion, progress, evidence, conclusion}, " "),
		Messages:     []llm.Message{{Role: "system", Content: "Stable goal work item fixture."}, {Role: "user", Content: strings.Join([]string{objective, criterion, progress, evidence, conclusion}, " ")}},
		ProviderName: "fixture", Model: "fixture",
	}
	outcome, err := (conversation.GoalSocketClient{Socket: socket}).RunGoal(ctx, request)
	if err != nil || outcome.Status != agent.RunCompleted {
		t.Fatalf("real Goal work item outcome = %+v, %v", outcome, err)
	}
	seenGoalContext := strings.Builder{}
	for _, message := range provider.roundMessages(2) {
		seenGoalContext.WriteString(message.Content)
		seenGoalContext.WriteByte('\n')
	}
	if !strings.Contains(seenGoalContext.String(), memoryBody) {
		t.Fatalf("Goal model request did not receive recalled memory: %s", seenGoalContext.String())
	}
	for _, fact := range []string{objective, criterion, progress, evidence, conclusion, "USER_GUIDANCE_M08", "PROJECT_GUIDANCE_M08", "NESTED_GUIDANCE_M08", "WORK_GUIDANCE_M08"} {
		if !strings.Contains(seenGoalContext.String(), fact) {
			t.Fatalf("Goal model request did not include fact fixture %q", fact)
		}
	}

	select {
	case input := <-extractSeen:
		if input.WorkKind != "goal" || len(input.Messages) != 1 || input.Messages[0].Text != preference || input.Messages[0].Kind != "goal_reply" {
			t.Fatalf("goal extraction input included non-user-reply facts: %+v", input)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goal memory extraction did not run")
	}
	if err = waitMemoryEntry(ctx, manager, root, "GoalReply"); err != nil {
		t.Fatalf("Goal run memory extraction did not persist: %v", err)
	}
	entries, err := manager.List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	foundPreference := false
	for _, entry := range entries {
		body, readErr := manager.Read(ctx, root, entry.Scope, entry.Filename)
		if readErr != nil {
			t.Fatal(readErr)
		}
		foundPreference = foundPreference || body.Body == memoryBody
		for _, fact := range []string{objective, criterion, progress, evidence, conclusion} {
			if strings.Contains(body.Body, fact) || strings.Contains(entry.Name, fact) || strings.Contains(entry.Description, fact) {
				t.Fatalf("goal fact %q was persisted as memory: %+v", fact, entry)
			}
		}
	}
	if !foundPreference {
		t.Fatalf("allowlisted user preference was not persisted: %+v", entries)
	}
	goalAfter, err := db.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(goalBefore.Goal, goalAfter.Goal) || !reflect.DeepEqual(goalBefore.Evidence, goalAfter.Evidence) {
		t.Fatalf("memory lifecycle changed Stable goal facts: before=%+v after=%+v", goalBefore, goalAfter)
	}
}

func TestM08MemorySurvivesCompactionRestoreAndCandidateRewind(t *testing.T) {
	ctx := context.Background()
	userConfig := filepath.Join(t.TempDir(), "config")
	stateDir := filepath.Join(t.TempDir(), "memory-state")
	deferCloseManager := func(manager memory.Manager) { _ = closeM08MemoryManager(context.Background(), manager) }

	// A real agent run first emits a large tool exchange, then crosses the context
	// threshold before its next provider call. The resulting run boundary and
	// summary survive a service restart, and a later Goal run recalls memory.
	root := t.TempDir()
	compactionStore, err := memory.NewStore(root, userConfig)
	if err != nil {
		t.Fatal(err)
	}
	compactionHeader, err := compactionStore.Save(memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeUser, Type: memory.TypeUser, Name: "Compaction", Description: "Persistent memory", Body: "COMPACTION_MEMORY_BODY_79c"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := memory.NewManager(memory.Options{ProjectRoot: root, UserConfigDir: userConfig, StateDir: stateDir, Selector: m08Selector{refs: []memory.MemoryRef{{Scope: memory.ScopeUser, Filename: compactionHeader.Filename}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { deferCloseManager(manager) }()
	compactionDB := mustOpenStore(t)
	info, err := sessionlog.Create(root, "memory compaction restore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = compactionDB.CreateGoal(ctx, core.Goal{
		ID: "goal-memory-compaction", Objective: "Preserve the user preference", Criteria: []core.Criterion{{ID: "criterion-memory", Kind: "manual", Payload: json.RawMessage(`{"marker":"memory survives compaction"}`)}},
		AllowedRoot: root, SourceSessionID: info.ID, AllowedCapabilities: []string{"read_file"}, Status: core.GoalActive,
	}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "compact.sock")
	provider := &m06Agent{}
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("m08-context-tool", "read_file", map[string]any{"path": strings.Repeat("x", 45000)})
		default:
			return m06TextRound("Goal work completed.")
		}
	}
	toolSchemas := []llm.ToolSchema{{Name: "read_file", Description: "Read a fixture file", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}
	contextManager, _ := sessioncontext.NewManager(16384, provider)
	runner := agent.NewRunner(provider, agent.RunnerOptions{
		MaxRetries: -1, ToolSchemas: toolSchemas, ContextManager: contextManager,
		ExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{Script: []agent.ToolOutcome{{Content: strings.Repeat("tool result context marker ", 740)}}}},
	})
	server := m05Serve(t, conversation.Deps{
		Store: compactionDB, ProjectRoot: root, ChatProvider: provider, Runner: runner, ProviderName: "fixture", Model: "fixture",
		Memory: conversation.NewMemoryGate(manager, root),
	}, socket)
	request := agent.ExecutionRequest{
		RunID: "m08-compaction-run", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: info.ID, GoalID: "goal-memory-compaction", WorkItemID: "item-memory-compaction"},
		Intent: "exercise real run compaction while preserving memory", ProviderName: "fixture", Model: "fixture",
		Messages: []llm.Message{{Role: "user", Content: strings.Repeat("earlier goal context ", 300)}},
	}
	if outcome, runErr := (conversation.GoalSocketClient{Socket: socket}).RunGoal(ctx, request); runErr != nil || outcome.Status != agent.RunCompleted {
		t.Fatalf("Goal run crossing compaction threshold = %+v, %v", outcome, runErr)
	}
	transcript, err := sessionlog.Replay(root, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	boundarySeen := false
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventBoundary {
			continue
		}
		boundary, ok := event.Data.(sessionlog.Boundary)
		if ok && boundary.Scope == sessionlog.BoundaryScopeRun && boundary.RunID == request.RunID && boundary.FromSeq > 0 && boundary.ToSeq > 0 {
			boundarySeen = true
			if !strings.Contains(boundary.Summary, "好的，已记录。") {
				t.Fatalf("unexpected real-run compaction summary: %+v", boundary)
			}
		}
	}
	if !boundarySeen {
		t.Fatal("agent runner did not persist a positive-sequence run compaction boundary")
	}
	if !m08RoundContains(provider.roundMessages(2), "Earlier conversation summary: 好的，已记录。") {
		t.Fatalf("provider's post-compaction round omitted the real run summary: %+v", provider.roundMessages(2))
	}
	if err = server.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = server.svc.Close(); err != nil {
		t.Fatal(err)
	}
	server.stop()
	if err = closeM08MemoryManager(ctx, manager); err != nil {
		t.Fatal(err)
	}
	manager, err = memory.NewManager(memory.Options{ProjectRoot: root, UserConfigDir: userConfig, StateDir: stateDir, Selector: m08Selector{refs: []memory.MemoryRef{{Scope: memory.ScopeUser, Filename: compactionHeader.Filename}}}})
	if err != nil {
		t.Fatal(err)
	}
	restartedSocket := filepath.Join(t.TempDir(), "restored.sock")
	restartedContextManager, _ := sessioncontext.NewManager(16384, provider)
	restartedRunner := agent.NewRunner(provider, agent.RunnerOptions{MaxRetries: -1, ContextManager: restartedContextManager})
	restarted := m05Serve(t, conversation.Deps{
		Store: compactionDB, ProjectRoot: root, ChatProvider: provider, Runner: restartedRunner, ProviderName: "fixture", Model: "fixture",
		Memory: conversation.NewMemoryGate(manager, root),
	}, restartedSocket)
	loaded := m05Call(t, restarted, conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: info.ID})
	var restored *sessionlog.Transcript
	for _, message := range loaded {
		if message.Type == "transcript" {
			restored = message.Transcript
		}
	}
	hasRestoredSummary := false
	if restored != nil {
		for _, item := range sessionlog.Project(*restored).Items {
			if item.Kind == sessionlog.ItemSummary && item.Summary != nil && strings.Contains(item.Summary.Summary, "好的，已记录。") {
				hasRestoredSummary = true
			}
		}
	}
	if !hasRestoredSummary {
		t.Fatalf("compacted transcript did not restore after service restart: %+v", loaded)
	}
	provider.respond = func(_ int, _ llm.Request) []llm.Event { return m06TextRound("memory recalled after compaction") }
	request.RunID = "m08-memory-after-compaction"
	request.Messages = []llm.Message{{Role: "user", Content: "continue with the recalled preference"}}
	if outcome, runErr := (conversation.GoalSocketClient{Socket: restartedSocket}).RunGoal(ctx, request); runErr != nil || outcome.Status != agent.RunCompleted {
		t.Fatalf("restored Goal run = %+v, %v", outcome, runErr)
	}
	postRestartMessages := provider.roundMessages(3)
	if !m08RoundContains(postRestartMessages, "COMPACTION_MEMORY_BODY_79c") {
		t.Fatalf("restored Goal run did not receive recalled memory: %+v", postRestartMessages)
	}
	if err = restarted.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = restarted.svc.Close(); err != nil {
		t.Fatal(err)
	}
	restarted.stop()
	if err = closeM08MemoryManager(ctx, manager); err != nil {
		t.Fatal(err)
	}

	// Rewind a real isolated candidate through the conversation protocol and
	// prove that PrepareRun still selects the persisted memory afterwards.
	db := mustOpenStore(t)
	rewind := m05NewRewindFixture(t, db, filepath.Join(t.TempDir(), "candidates"))
	rewindStore, err := memory.NewStore(rewind.root, userConfig)
	if err != nil {
		t.Fatal(err)
	}
	rewindHeader, err := rewindStore.Save(memory.MemoryChange{Action: memory.ActionUpsert, Scope: memory.ScopeProject, Type: memory.TypeReference, Name: "Rollback", Description: "Survives candidate rewind", Body: "REWIND_MEMORY_BODY_4ac"})
	if err != nil {
		t.Fatal(err)
	}
	rewindManager, err := memory.NewManager(memory.Options{ProjectRoot: rewind.root, UserConfigDir: userConfig, StateDir: stateDir, Selector: m08Selector{refs: []memory.MemoryRef{{Scope: memory.ScopeProject, Filename: rewindHeader.Filename}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rewindManager.Close(context.Background())
	before, err := rewindManager.PrepareRun(ctx, rewind.root, rewind.root, rewind.sessionID, "candidate work")
	if err != nil || len(before.Selected) != 1 || before.Selected[0].Body != "REWIND_MEMORY_BODY_4ac" {
		t.Fatalf("baseline memory recall before candidate rewind = %+v, %v", before, err)
	}
	snapshots, err := candidate.NewSnapshotStore(rewind.root, 1<<22, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewindSocket := filepath.Join(t.TempDir(), "rewind.sock")
	rewindServer := m05Serve(t, conversation.Deps{Store: db, ProjectRoot: rewind.root, Snapshots: snapshots, ChatProvider: &m05Chat{}, Memory: conversation.NewMemoryGate(rewindManager, rewind.root)}, rewindSocket)
	rewound := m05Call(t, rewindServer, conversation.ClientMsg{Op: "snapshot_rewind", SessionID: rewind.sessionID, CandidateID: "cand-1", SnapshotID: rewind.snapV1.SnapshotID, CandidateDigest: rewind.currentDigest})
	if errText := m05Error(rewound); errText != "" {
		t.Fatalf("candidate snapshot rewind failed: %s", errText)
	}
	after, err := rewindManager.PrepareRun(ctx, rewind.root, rewind.root, rewind.sessionID, "candidate work")
	if err != nil || len(after.Selected) != 1 || after.Selected[0].Body != "REWIND_MEMORY_BODY_4ac" {
		t.Fatalf("memory was not recalled after candidate rewind: %+v, %v", after, err)
	}
	if err = rewindServer.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = rewindServer.svc.Close(); err != nil {
		t.Fatal(err)
	}
	rewindServer.stop()
}

func mustOpenStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func m08RoundContains(messages []llm.Message, text string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}

func closeM08MemoryManager(ctx context.Context, manager memory.Manager) error {
	closer, ok := manager.(interface{ Close(context.Context) error })
	if !ok {
		return nil
	}
	return closer.Close(ctx)
}
