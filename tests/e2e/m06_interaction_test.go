package e2e

// M06 end-to-end evidence: custom slash commands with hot reload and prompt
// expansion, the plan-mode approval state machine with the plan-file direct
// write and post-approval accept-edits runs, the ask_user question round trip
// with leftover-question queueing, the per-session todo list with redaction
// and restart recovery, and the proposal confirm/reject protocol state
// machines. Everything drives the real conversation service over its unix
// socket with a scripted streaming provider; the only sandbox use is one
// accept-edits candidate write through the real tool helper (bwrap).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/planfile"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/tools"
	"stable/internal/tui"
)

// ---------------------------------------------------------------------------
// Scripted streaming provider shared by every scenario.
// ---------------------------------------------------------------------------

// m06Agent is the fake streaming model: each provider round is scripted by
// the owning test through respond(call, request). Every round's model-visible
// messages are recorded so tests can assert what the agent actually saw.
type m06Agent struct {
	mu      sync.Mutex
	calls   int
	seen    [][]llm.Message
	respond func(call int, req llm.Request) []llm.Event
}

func (p *m06Agent) Stream(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.seen = append(p.seen, append([]llm.Message(nil), request.Messages...))
	respond := p.respond
	p.mu.Unlock()
	events := []llm.Event{
		{Kind: llm.TextDelta, Text: "M06 script exhausted"},
		{Kind: llm.StreamEnd},
	}
	if respond != nil {
		events = respond(call, request)
	}
	out := make(chan llm.Event, len(events))
	errs := make(chan error, 1)
	for _, event := range events {
		out <- event
	}
	close(out)
	close(errs)
	return out, errs
}

// GenerateChat lets the same fake serve the plain chat path.
func (p *m06Agent) GenerateChat(_ context.Context, _ []decision.ChatMessage) (string, error) {
	return "好的，已记录。", nil
}

// reset clears the round counter so a new run scripts from round 1 again.
func (p *m06Agent) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = 0
	p.seen = nil
}

func (p *m06Agent) roundCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// roundMessages returns the model-visible messages of one 1-based round.
func (p *m06Agent) roundMessages(call int) []llm.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 1 || call > len(p.seen) {
		return nil
	}
	return p.seen[call-1]
}

func m06TextRound(text string) []llm.Event {
	return []llm.Event{
		{Kind: llm.TextDelta, Text: text},
		{Kind: llm.StreamEnd},
	}
}

func m06ToolRound(id, name string, args map[string]any) []llm.Event {
	raw, _ := json.Marshal(args)
	return []llm.Event{
		{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: id, Name: name, Arguments: raw, Complete: true}},
		{Kind: llm.StreamEnd},
	}
}

// ---------------------------------------------------------------------------
// Service harness: real conversation service, real permission gate, real
// executor factory with the M06 interaction sinks — the chatserve wiring.
// ---------------------------------------------------------------------------

type m06Env struct {
	root   string // project root
	db     *store.Store
	agent  *m06Agent
	socket string
	svc    *conversation.Service
	cancel context.CancelFunc
}

// m06NewService wires and starts one conversation service with the production
// dependency set (store gate, snapshots, question/plan/todo sinks). The
// helper path may be empty for scenarios that never touch the sandbox.
func m06NewService(t *testing.T, root string, db *store.Store, provider *m06Agent, structured decision.StructuredProvider, credential, helperPath string) *m06Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var credentials []string
	if credential != "" {
		credentials = []string{credential}
	}
	snapshots, err := candidate.NewSnapshotStore(root, 1<<22, 20, credentials)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	askSink := conversation.NewAskAdapter(nil)
	todoProvider := conversation.NewTodoProvider(nil)
	planSink := conversation.NewPlanApprovalSink(nil)
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Sandbox:            sandbox.New(),
		Gate:               execution.StorePermissionGate{Store: db},
		Approvals:          db,
		Candidates:         db,
		HelperPath:         helperPath,
		SessionRoot:        root,
		ProviderCredential: credential,
		Snapshots:          snapshots,
		QuestionSink:       askSink,
		TodoProvider:       todoProvider,
	}, execution.WithPlanSink(planSink))
	runner := agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: m06ToolSchemas(), MaxRetries: -1})
	// The socket must live outside the project root: candidate manifests
	// reject special files, mirroring the runtime layout where chat.sock sits
	// in the state directory rather than the project share.
	socket := filepath.Join(filepath.Dir(root), fmt.Sprintf("chat-%d.sock", time.Now().UnixNano()))
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, Provider: structured, ChatProvider: provider, Runner: runner,
		ProviderCredential: credential, ProviderName: "fixture", Model: "fixture",
		ProjectRoot: root, SocketPath: socket, PollEvery: 200 * time.Millisecond,
		Snapshots: snapshots,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	askSink.Bind(svc)
	todoProvider.Bind(svc)
	planSink.Bind(svc)
	env := &m06Env{root: root, db: db, agent: provider, socket: socket, svc: svc, cancel: cancel}
	t.Cleanup(func() {
		svc.Close()
		cancel()
	})
	return env
}

// m06NewProject creates a fresh project root and its state store.
func m06NewProject(t *testing.T) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return root, db
}

// m06ToolSchemas mirrors the chatserve whitelist: default registry schemas
// plus the six M06 interaction tools, sorted by name.
func m06ToolSchemas() []llm.ToolSchema {
	nameMap := map[string]string{
		"read_file":      "read_file",
		"write_file":     "write_file",
		"edit_file":      "edit_file",
		"glob":           "glob",
		"grep":           "grep",
		"ask_user":       "ask_user",
		"exit_plan_mode": "exit_plan_mode",
		"task_create":    "task_create",
		"task_get":       "task_get",
		"task_list":      "task_list",
		"task_update":    "task_update",
	}
	registry := tools.CreateDefaultTools().Registry
	sources := append(append([]map[string]any{}, registry.GetAllSchemas()...), execution.M06ToolSchemas()...)
	schemas := make([]llm.ToolSchema, 0, len(nameMap)+1)
	for _, schema := range sources {
		internalName, _ := schema["name"].(string)
		name, ok := nameMap[internalName]
		if !ok {
			continue
		}
		description, _ := schema["description"].(string)
		input, _ := schema["input_schema"].(map[string]any)
		schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
	}
	schemas = append(schemas, llm.ToolSchema{
		Name:        "command",
		Description: tools.BashDescription,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
	})
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

var m06Helper struct {
	once sync.Once
	path string
	err  error
}

// m06HelperPath returns the agentworker tool-helper binary used inside the
// bwrap sandbox. The e2e shell script exports STABLE_M06_HELPER; a standalone
// go test invocation builds the helper itself.
func m06HelperPath(t *testing.T) string {
	t.Helper()
	if helper := os.Getenv("STABLE_M06_HELPER"); helper != "" {
		if _, err := os.Stat(helper); err == nil {
			return helper
		}
	}
	m06Helper.once.Do(func() {
		dir, err := os.MkdirTemp("", "stable-m06-helper-")
		if err != nil {
			m06Helper.err = err
			return
		}
		out := filepath.Join(dir, "agentworker")
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, "./cmd/agentworker")
		cmd.Dir = "../.."
		if buf, err := cmd.CombinedOutput(); err != nil {
			m06Helper.err = fmt.Errorf("build agentworker helper: %v: %s", err, buf)
			return
		}
		m06Helper.path = out
	})
	if m06Helper.err != nil {
		t.Fatal(m06Helper.err)
	}
	return m06Helper.path
}

// ---------------------------------------------------------------------------
// Protocol and session-log assertion helpers.
// ---------------------------------------------------------------------------

func m06Op(t *testing.T, ctx context.Context, socket string, msg conversation.ClientMsg) []conversation.ServerMsg {
	t.Helper()
	out, err := conversation.Request(ctx, socket, msg)
	if err != nil {
		t.Fatalf("op %s failed: %v", msg.Op, err)
	}
	return out
}

func m06SessionCreate(t *testing.T, ctx context.Context, env *m06Env) string {
	t.Helper()
	msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: env.root})
	for _, m := range msgs {
		if m.Type == "session" && m.Session != nil {
			return m.Session.ID
		}
	}
	t.Fatalf("no session in %+v", msgs)
	return ""
}

// m06RunStream reads one run's message stream in the background so the test
// can act (reply, resolve an approval) while the run blocks in a sink.
type m06RunStream struct {
	client *conversation.StreamClient
	mu     sync.Mutex
	msgs   []conversation.ServerMsg
	done   chan struct{}
}

func m06StartRun(t *testing.T, ctx context.Context, env *m06Env, sessionID, runID string) *m06RunStream {
	t.Helper()
	request := agent.ExecutionRequest{
		RunID:    runID,
		Work:     agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent:   "M06 e2e run",
		Messages: []llm.Message{{Role: "user", Content: "M06 e2e run"}},
		Model:    "fixture",
	}
	client, err := conversation.OpenRun(ctx, env.socket, request)
	if err != nil {
		t.Fatal(err)
	}
	stream := &m06RunStream{client: client, done: make(chan struct{})}
	go func() {
		defer close(stream.done)
		for {
			message, err := client.Receive()
			if err != nil {
				return
			}
			stream.mu.Lock()
			stream.msgs = append(stream.msgs, message)
			stream.mu.Unlock()
		}
	}()
	return stream
}

func (s *m06RunStream) messages() []conversation.ServerMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]conversation.ServerMsg(nil), s.msgs...)
}

func (s *m06RunStream) close() {
	s.client.Close()
	<-s.done
}

// waitOutcome blocks until the run reports its outcome and closes the stream.
func (s *m06RunStream) waitOutcome(t *testing.T, timeout time.Duration) agent.RunOutcome {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range s.messages() {
			if m.Type == "run_outcome" && m.Outcome != nil {
				s.close()
				return *m.Outcome
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.close()
	t.Fatalf("timed out waiting for the run outcome; messages: %s", m06Dump(s.messages()))
	return agent.RunOutcome{}
}

// assertNoErrors fails when the stream carried an error message.
func (s *m06RunStream) assertNoErrors(t *testing.T) {
	t.Helper()
	for _, m := range s.messages() {
		if m.Type == "error" {
			t.Fatalf("unexpected stream error: %s", m.Error)
		}
	}
}

// hasType reports whether the stream carried a message of the given type.
func (s *m06RunStream) hasType(typ string) bool {
	for _, m := range s.messages() {
		if m.Type == typ {
			return true
		}
	}
	return false
}

func m06Dump(msgs []conversation.ServerMsg) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, fmt.Sprintf("{%s error:%q}", m.Type, m.Error))
	}
	return strings.Join(parts, " ")
}

func m06Poll(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func m06Transcript(t *testing.T, root, sessionID string) sessionlog.Transcript {
	t.Helper()
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return transcript
}

func m06EventsOfType(t *testing.T, root, sessionID, typ string) []sessionlog.Event {
	t.Helper()
	var out []sessionlog.Event
	for _, event := range m06Transcript(t, root, sessionID).Events {
		if event.Type == typ {
			out = append(out, event)
		}
	}
	return out
}

// m06Decode round-trips one event payload into its typed struct.
func m06Decode(t *testing.T, data any, target any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func m06MessageTexts(t *testing.T, root, sessionID string) []sessionlog.Message {
	t.Helper()
	var out []sessionlog.Message
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventMessage) {
		var message sessionlog.Message
		m06Decode(t, event.Data, &message)
		out = append(out, message)
	}
	return out
}

// m06ToolTrace pairs session-log tool_call events with their results.
type m06ToolTrace struct {
	calls   map[string]sessionlog.ToolCall
	results map[string]sessionlog.ToolResult
	order   []string
}

func m06ToolTraceOf(t *testing.T, root, sessionID string) m06ToolTrace {
	t.Helper()
	trace := m06ToolTrace{calls: map[string]sessionlog.ToolCall{}, results: map[string]sessionlog.ToolResult{}}
	for _, event := range m06Transcript(t, root, sessionID).Events {
		switch event.Type {
		case sessionlog.EventToolCall:
			var call sessionlog.ToolCall
			m06Decode(t, event.Data, &call)
			if _, seen := trace.calls[call.CallID]; !seen {
				trace.order = append(trace.order, call.CallID)
			}
			trace.calls[call.CallID] = call
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			m06Decode(t, event.Data, &result)
			trace.results[result.CallID] = result
		}
	}
	return trace
}

// m06ResultText returns the tool result content as text; the executor always
// stores strings for these tools.
func m06ResultText(t *testing.T, result sessionlog.ToolResult) string {
	t.Helper()
	content, ok := result.Result.(string)
	if !ok {
		t.Fatalf("tool result is not text: %+v", result)
	}
	return content
}

// resultOf returns the tool result content of the first call with the name.
func (tr m06ToolTrace) resultOf(t *testing.T, name string) sessionlog.ToolResult {
	t.Helper()
	for _, callID := range tr.order {
		if tr.calls[callID].Name == name {
			return tr.results[callID]
		}
	}
	t.Fatalf("no tool call %q in the session log", name)
	return sessionlog.ToolResult{}
}

func m06PlanApprovalEvents(t *testing.T, root, sessionID string) []sessionlog.PlanApprovalRecord {
	t.Helper()
	var out []sessionlog.PlanApprovalRecord
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventPlanApproval) {
		var record sessionlog.PlanApprovalRecord
		m06Decode(t, event.Data, &record)
		out = append(out, record)
	}
	return out
}

func m06PlanModeEvents(t *testing.T, root, sessionID string) []sessionlog.PlanMode {
	t.Helper()
	var out []sessionlog.PlanMode
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventPlanMode) {
		var record sessionlog.PlanMode
		m06Decode(t, event.Data, &record)
		out = append(out, record)
	}
	return out
}

func m06LoadPlanState(t *testing.T, ctx context.Context, env *m06Env, sessionID string) conversation.PlanState {
	t.Helper()
	msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "session_load", ProjectRoot: env.root, SessionID: sessionID})
	for _, m := range msgs {
		if m.Type == "transcript" && m.Plan != nil {
			return *m.Plan
		}
	}
	t.Fatalf("session_load response without plan state: %s", m06Dump(msgs))
	return conversation.PlanState{}
}

func m06Questions(t *testing.T, ctx context.Context, env *m06Env, sessionID string) []sessionlog.PendingQuestion {
	t.Helper()
	msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "question_list", SessionID: sessionID})
	for _, m := range msgs {
		if m.Type == "questions" {
			return m.Questions
		}
	}
	t.Fatalf("question_list response without questions: %s", m06Dump(msgs))
	return nil
}

func m06CandidateRoot(root, sessionID, runID string) string {
	return filepath.Join(filepath.Dir(root), ".stable-candidates", "session-"+sessionID+"-"+runID)
}

// ---------------------------------------------------------------------------
// Scenario a: custom commands — completion, expansion, hot reload.
// ---------------------------------------------------------------------------

func TestM06CustomCommandsHotReloadAndExpansion(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	env := m06NewService(t, root, db, &m06Agent{}, nil, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	cmdDir := filepath.Join(root, ".stable", "commands")
	if err := os.MkdirAll(cmdDir, 0700); err != nil {
		t.Fatal(err)
	}
	checklistBody := "请按检查单复核 $ARGUMENTS，并给出结论。\n"
	checklistFile := filepath.Join(cmdDir, "review-checklist.md")
	if err := os.WriteFile(checklistFile, []byte("---\ndescription: 按 Stable 检查单复核走线\nargument-hint: <对象> <约束>\naliases: [checklist]\n---\n"+checklistBody), 0600); err != nil {
		t.Fatal(err)
	}

	// Completion comes from the loader + registry building blocks the TUI
	// wires (T12); commands never pass through the session service itself.
	loader := commands.NewLoader(cmdDir, "")
	cmds, rejected, err := loader.Commands()
	if err != nil || len(rejected) != 0 {
		t.Fatalf("load commands: err=%v rejected=%v", err, rejected)
	}
	if len(cmds) != 1 || cmds[0].Name != "review-checklist" {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].Description != "按 Stable 检查单复核走线" || cmds[0].ArgPrompt != "<对象> <约束>" {
		t.Fatalf("frontmatter = %+v", cmds[0])
	}
	var found bool
	for _, item := range tui.CommandItems(cmds) {
		if item.Label == "/review-checklist" && item.Detail == "按 Stable 检查单复核走线" && item.InsertText == "/review-checklist " {
			found = true
		}
	}
	if !found {
		t.Fatal("completion list does not contain the custom command")
	}

	// Dispatch replicates the TUI path: registry lookup, $ARGUMENTS expansion,
	// then the expanded prompt enters the session as the run's user message.
	registry := commands.NewRegistry()
	for _, cmd := range cmds {
		if !registry.RegisterOptional(cmd) {
			t.Fatalf("custom command %q not registered", cmd.Name)
		}
	}
	name, args := commands.Parse("/review-checklist 走线宽度 8mil")
	if name != "review-checklist" || args != "走线宽度 8mil" {
		t.Fatalf("parse = %q %q", name, args)
	}
	command, ok := registry.Find(name)
	if !ok {
		t.Fatal("registry lost the custom command")
	}
	if alias, aliasOK := registry.Find("checklist"); !aliasOK || alias.Name != "review-checklist" {
		t.Fatal("frontmatter alias is not resolvable")
	}
	expanded := commands.ExpandPrompt(command.Body, args)
	if expanded != "请按检查单复核 走线宽度 8mil，并给出结论。" {
		t.Fatalf("expansion = %q", expanded)
	}
	runID := "run-cmd-1"
	stream := m06StartRunWithText(t, ctx, env, sessionID, runID, expanded)
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("command run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)
	texts := m06MessageTexts(t, root, sessionID)
	if len(texts) == 0 || texts[0].Text != expanded {
		t.Fatalf("session messages = %+v, want the expanded prompt first", texts)
	}

	// Hot update: a second command file appears without any restart, and a
	// template without $ARGUMENTS appends the arguments as a user request.
	notesBody := "请把以下内容整理为项目备注。"
	if err := os.WriteFile(filepath.Join(cmdDir, "notes.md"), []byte("---\ndescription: 记录项目备注\n---\n"+notesBody+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmds, rejected, err = loader.Commands()
	if err != nil || len(rejected) != 0 {
		t.Fatalf("hot reload after add: err=%v rejected=%v", err, rejected)
	}
	names := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		names = append(names, cmd.Name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "notes" || names[1] != "review-checklist" {
		t.Fatalf("hot-reloaded commands = %v", names)
	}
	var notes *commands.Command
	for _, cmd := range cmds {
		if cmd.Name == "notes" {
			notes = cmd
		}
	}
	if got := commands.ExpandPrompt(notes.Body, "补齐丝印"); got != notesBody+"\n\n## User Request\n\n补齐丝印" {
		t.Fatalf("no-placeholder expansion = %q", got)
	}

	// Deletion is equally visible without a restart: the TUI rebuilds its
	// registry from the loader on every refresh, so a fresh rebuild loses the
	// deleted command and keeps the surviving one.
	if err := os.Remove(checklistFile); err != nil {
		t.Fatal(err)
	}
	m06Poll(t, 10*time.Second, "deleted command to disappear from the registry", func() bool {
		cmds, _, err := loader.Commands()
		if err != nil || len(cmds) != 1 || cmds[0].Name != "notes" {
			return false
		}
		fresh := commands.NewRegistry()
		for _, cmd := range cmds {
			fresh.RegisterOptional(cmd)
		}
		if _, stillThere := fresh.Find("review-checklist"); stillThere {
			return false
		}
		_, present := fresh.Find("notes")
		return present
	})
}

// m06StartRunWithText starts a session run whose user message is text — the
// exact request shape the TUI submits for a KindPrompt command.
func m06StartRunWithText(t *testing.T, ctx context.Context, env *m06Env, sessionID, runID, text string) *m06RunStream {
	t.Helper()
	request := agent.ExecutionRequest{
		RunID:    runID,
		Work:     agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent:   text,
		Messages: []llm.Message{{Role: "user", Content: text}},
		Model:    "fixture",
	}
	client, err := conversation.OpenRun(ctx, env.socket, request)
	if err != nil {
		t.Fatal(err)
	}
	stream := &m06RunStream{client: client, done: make(chan struct{})}
	go func() {
		defer close(stream.done)
		for {
			message, err := client.Receive()
			if err != nil {
				return
			}
			stream.mu.Lock()
			stream.msgs = append(stream.msgs, message)
			stream.mu.Unlock()
		}
	}()
	return stream
}

// ---------------------------------------------------------------------------
// Scenario b: plan mode, plan-file direct write, approval auto, accept-edits.
// ---------------------------------------------------------------------------

func TestM06PlanApprovalAutoAndAcceptEdits(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	helper := m06HelperPath(t)
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, "", helper)
	sessionID := m06SessionCreate(t, ctx, env)
	runID := "run-plan-1"

	planPath, err := planfile.PlanPath(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	planContent := "# 走线修复计划\n\n1. 复核约束\n2. 生成候选\n"

	// Enter plan mode through the protocol op.
	msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "plan_mode", SessionID: sessionID})
	var state *conversation.PlanState
	for _, m := range msgs {
		if m.Type == "plan_state" {
			state = m.PlanState
		}
	}
	if state == nil || state.Mode != "plan" || state.PlanPath != planPath {
		t.Fatalf("plan_mode response = %+v", state)
	}
	if _, statErr := os.Lstat(planPath); statErr != nil {
		t.Fatalf("plan file not created: %v", statErr)
	}
	if modes := m06PlanModeEvents(t, root, sessionID); len(modes) != 1 || modes[0].Mode != "plan" || modes[0].Reason != "user_toggle" {
		t.Fatalf("plan_mode events = %+v", modes)
	}

	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-write", "write_file", map[string]any{"file_path": ".stable/plans/" + sessionID + ".md", "content": planContent})
		case 2:
			return m06ToolRound("call-exit", "exit_plan_mode", map[string]any{})
		default:
			return m06TextRound("计划已确认。")
		}
	}
	provider.mu.Unlock()

	stream := m06StartRun(t, ctx, env, sessionID, runID)
	// exit_plan_mode blocks in the plan sink until the approval is resolved.
	m06Poll(t, 30*time.Second, "plan approval submission event", func() bool {
		for _, record := range m06PlanApprovalEvents(t, root, sessionID) {
			if record.Status == sessionlog.PlanApprovalSubmitted {
				return true
			}
		}
		return false
	})
	if !stream.hasType("plan_approval_pending") {
		t.Fatal("run stream did not receive the plan_approval_pending push")
	}
	resolve := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "plan_resolve", SessionID: sessionID, ApprovalChoice: conversation.PlanResolveAuto})
	var resolvedState *conversation.PlanState
	for _, m := range resolve {
		if m.Type == "plan_state" {
			resolvedState = m.PlanState
		}
	}
	if resolvedState == nil || resolvedState.Mode != "default" || resolvedState.ExecutionMode != conversation.PlanExecutionAcceptEdits {
		t.Fatalf("plan_resolve response state = %+v", resolvedState)
	}
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("plan run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)

	// The plan write never entered a candidate: the plan file holds the
	// content and no candidate workspace was created for the run.
	content, err := os.ReadFile(planPath)
	if err != nil || string(content) != planContent {
		t.Fatalf("plan file = %q, %v", content, err)
	}
	if _, err := os.Lstat(m06CandidateRoot(root, sessionID, runID)); !os.IsNotExist(err) {
		t.Fatalf("candidate workspace exists for a plan-only run: %v", err)
	}
	if _, err := db.GetCandidate(ctx, "session-"+sessionID+"-"+runID); err == nil {
		t.Fatal("a candidate is registered for a plan-only run")
	}

	// Session log audit trail: plan approval submitted once, resolved auto,
	// the plan-mode exit with its reason, and the inserted user message.
	records := m06PlanApprovalEvents(t, root, sessionID)
	if len(records) != 2 || records[0].Status != sessionlog.PlanApprovalSubmitted || records[1].Status != sessionlog.PlanApprovalApprovedAuto || records[0].RequestID != records[1].RequestID {
		t.Fatalf("plan approval events = %+v", records)
	}
	if records[0].PlanPath != planPath {
		t.Fatalf("approval plan path = %q", records[0].PlanPath)
	}
	modes := m06PlanModeEvents(t, root, sessionID)
	if len(modes) != 2 || modes[1].Mode != "default" || modes[1].Reason != "plan_approved" {
		t.Fatalf("plan mode events = %+v", modes)
	}
	texts := m06MessageTexts(t, root, sessionID)
	var approvalMessage bool
	for _, message := range texts {
		if message.Role == "user" && message.Text == "计划已批准，后续按自动接受模式执行" {
			approvalMessage = true
		}
	}
	if !approvalMessage {
		t.Fatalf("approval user message missing: %+v", texts)
	}

	trace := m06ToolTraceOf(t, root, sessionID)
	if result := trace.resultOf(t, "write_file"); result.Error != "" || m06ResultText(t, result) != "计划文件已更新" {
		t.Fatalf("plan write result = %+v", result)
	}
	if result := trace.resultOf(t, "exit_plan_mode"); result.Error != "" || m06ResultText(t, result) != "计划已批准,请结束本回合,不要再调用任何工具" {
		t.Fatalf("exit_plan_mode result = %+v", result)
	}

	// A restart projects the same transcript; the plan state itself is
	// runtime state that dropped back to default with the accept-edits mode
	// recorded for the next run.
	restored := m06LoadPlanState(t, ctx, env, sessionID)
	if restored.Mode != "default" || restored.ExecutionMode != conversation.PlanExecutionAcceptEdits {
		t.Fatalf("restored plan state = %+v", restored)
	}

	// Post-approval runs write through the candidate with writes auto
	// accepted (acceptEdits), and the formal project stays untouched.
	acceptRun := "run-accept-1"
	provider.reset()
	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-notes", "write_file", map[string]any{"file_path": "notes.txt", "content": "候选区笔记"})
		default:
			return m06TextRound("已写入候选区。")
		}
	}
	provider.mu.Unlock()
	stream = m06StartRunWithText(t, ctx, env, sessionID, acceptRun, "把笔记写进候选区")
	if outcome := stream.waitOutcome(t, 120*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("accept-edits run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)
	trace = m06ToolTraceOf(t, root, sessionID)
	if result, ok := trace.results["call-notes"]; !ok || result.Error != "" || m06ResultText(t, result) == "" {
		t.Fatalf("candidate write result = %+v (present=%t)", result, ok)
	}
	if _, err := os.Lstat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("accept-edits write reached the formal project")
	}
	candidateNotes, err := os.ReadFile(filepath.Join(m06CandidateRoot(root, sessionID, acceptRun), "notes.txt"))
	if err != nil || string(candidateNotes) != "候选区笔记" {
		t.Fatalf("candidate notes = %q, %v", candidateNotes, err)
	}
	record, err := db.GetCandidate(ctx, "session-"+sessionID+"-"+acceptRun)
	if err != nil || record.Candidate.Status != "ready" {
		t.Fatalf("candidate record = %+v, %v", record, err)
	}
}

// ---------------------------------------------------------------------------
// Scenario b (feedback branch): feedback keeps the session in plan mode.
// ---------------------------------------------------------------------------

func TestM06PlanFeedbackKeepsPlanMode(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	if msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "plan_mode", SessionID: sessionID}); len(msgs) == 0 {
		t.Fatal("plan_mode returned no messages")
	}
	planContent := "# 初版计划\n"
	feedback := "请补充验证章节"

	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-write", "write_file", map[string]any{"file_path": ".stable/plans/" + sessionID + ".md", "content": planContent})
		case 2:
			return m06ToolRound("call-exit", "exit_plan_mode", map[string]any{})
		default:
			return m06TextRound("收到反馈，继续修改计划。")
		}
	}
	provider.mu.Unlock()

	runID := "run-plan-fb"
	stream := m06StartRun(t, ctx, env, sessionID, runID)
	m06Poll(t, 30*time.Second, "plan approval submission event", func() bool {
		for _, record := range m06PlanApprovalEvents(t, root, sessionID) {
			if record.Status == sessionlog.PlanApprovalSubmitted {
				return true
			}
		}
		return false
	})
	resolve := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "plan_resolve", SessionID: sessionID, ApprovalChoice: conversation.PlanResolveFeedback, Text: feedback})
	var resolvedState *conversation.PlanState
	var resolutionText string
	for _, m := range resolve {
		if m.Type == "plan_state" {
			resolvedState = m.PlanState
		}
		if m.Type == "message" && m.Message != nil {
			resolutionText = m.Message.Text
		}
	}
	if resolvedState == nil || resolvedState.Mode != "plan" {
		t.Fatalf("feedback resolve state = %+v", resolvedState)
	}
	if resolutionText != "用户要求继续修改计划："+feedback {
		t.Fatalf("feedback resolution message = %q", resolutionText)
	}
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("feedback run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)

	records := m06PlanApprovalEvents(t, root, sessionID)
	if len(records) != 2 || records[1].Status != sessionlog.PlanApprovalFeedback || records[1].Feedback != feedback {
		t.Fatalf("plan approval events = %+v", records)
	}
	// Plan mode is kept: only the user_toggle entry exists in the log.
	if modes := m06PlanModeEvents(t, root, sessionID); len(modes) != 1 || modes[0].Mode != "plan" {
		t.Fatalf("plan mode events = %+v", modes)
	}
	trace := m06ToolTraceOf(t, root, sessionID)
	if result := trace.resultOf(t, "exit_plan_mode"); result.Error != "" || m06ResultText(t, result) != "用户要求继续修改计划:"+feedback+",请保持计划模式" {
		t.Fatalf("exit_plan_mode result = %+v", result)
	}
	if state := m06LoadPlanState(t, ctx, env, sessionID); state.Mode != "plan" || state.ExecutionMode != "" {
		t.Fatalf("restored plan state = %+v", state)
	}
}

// ---------------------------------------------------------------------------
// Scenario b (negative): exit_plan_mode outside plan mode is an error result.
// ---------------------------------------------------------------------------

func TestM06ExitPlanModeOutsidePlanMode(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-exit", "exit_plan_mode", map[string]any{})
		default:
			return m06TextRound("好的。")
		}
	}
	provider.mu.Unlock()

	stream := m06StartRunWithText(t, ctx, env, sessionID, "run-no-plan", "请结束计划")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)
	trace := m06ToolTraceOf(t, root, sessionID)
	result := trace.resultOf(t, "exit_plan_mode")
	if result.Error != "Error: 当前不在计划模式" {
		t.Fatalf("exit_plan_mode outside plan mode = %+v", result)
	}
	if len(m06PlanModeEvents(t, root, sessionID)) != 0 {
		t.Fatal("plan_mode events written outside plan mode")
	}
}

// ---------------------------------------------------------------------------
// Scenario c: ask_user round trip and leftover-question queueing.
// ---------------------------------------------------------------------------

func TestM06AskUserReplyRoundTrip(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-ask", "ask_user", map[string]any{
				"questions": []any{map[string]any{
					"question": "采用哪种走线方案？",
					"header":   "方案",
					"options":  []any{map[string]any{"label": "A", "description": "直角"}, map[string]any{"label": "B", "description": "等长绕线"}},
				}},
			})
		default:
			return m06TextRound("收到答复，继续执行。")
		}
	}
	provider.mu.Unlock()

	stream := m06StartRunWithText(t, ctx, env, sessionID, "run-ask-1", "先问我一个方案问题")
	var questionID string
	m06Poll(t, 30*time.Second, "pending question", func() bool {
		for _, question := range m06Questions(t, ctx, env, sessionID) {
			if question.Status == sessionlog.QuestionPending {
				questionID = question.QuestionID
				return true
			}
		}
		return false
	})
	if !stream.hasType("questions") {
		t.Fatal("run stream did not receive the questions push")
	}
	if questions := m06Questions(t, ctx, env, sessionID); len(questions) != 1 || questions[0].Prompt == "" {
		t.Fatalf("questions = %+v", questions)
	}

	const answer = "选项B：等长绕线"
	replyMsgs, err := conversation.Request(ctx, env.socket, conversation.ClientMsg{Op: "reply", SessionID: sessionID, QuestionID: questionID, Text: answer})
	if err != nil {
		t.Fatalf("reply failed: %v", err)
	}
	var reply *sessionlog.QuestionReply
	for _, m := range replyMsgs {
		if m.Type == "reply" {
			reply = m.Reply
		}
	}
	if reply == nil || reply.ReplyText != answer {
		t.Fatalf("reply response = %+v", reply)
	}
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("ask run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)

	questions := m06Questions(t, ctx, env, sessionID)
	if len(questions) != 1 || questions[0].Status != sessionlog.QuestionReplied {
		t.Fatalf("questions after reply = %+v", questions)
	}
	trace := m06ToolTraceOf(t, root, sessionID)
	result := trace.resultOf(t, "ask_user")
	if result.Error != "" || !strings.Contains(m06ResultText(t, result), "Q: 采用哪种走线方案？") || !strings.Contains(m06ResultText(t, result), "A: "+answer) {
		t.Fatalf("ask_user result = %+v", result)
	}
	var sawEventQuestion, sawEventReply bool
	for _, event := range m06Transcript(t, root, sessionID).Events {
		if event.Type == sessionlog.EventQuestion {
			var q sessionlog.PendingQuestion
			m06Decode(t, event.Data, &q)
			sawEventQuestion = q.QuestionID == questionID && strings.Contains(q.Prompt, "[方案] 采用哪种走线方案？") && strings.Contains(q.Prompt, "- B：等长绕线")
		}
		if event.Type == sessionlog.EventReply {
			var r sessionlog.QuestionReply
			m06Decode(t, event.Data, &r)
			sawEventReply = r.QuestionID == questionID && r.ReplyText == answer
		}
	}
	if !sawEventQuestion || !sawEventReply {
		t.Fatalf("question/reply events missing: question=%t reply=%t", sawEventQuestion, sawEventReply)
	}

	// A replied question consumes exactly once.
	if _, err := conversation.Request(ctx, env.socket, conversation.ClientMsg{Op: "reply", SessionID: sessionID, QuestionID: questionID, Text: "重复"}); err == nil || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("duplicate reply = %v", err)
	}
}

func TestM06AskLeftoverReplyQueuedForNextRun(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		if call == 1 {
			return m06ToolRound("call-ask", "ask_user", map[string]any{
				"questions": []any{map[string]any{
					"question": "是否继续写入候选区？",
					"header":   "确认",
					"options":  []any{map[string]any{"label": "继续"}, map[string]any{"label": "停止"}},
				}},
			})
		}
		return m06TextRound("不应到达。")
	}
	provider.mu.Unlock()

	runID := "run-ask-cancel"
	stream := m06StartRunWithText(t, ctx, env, sessionID, runID, "先提问再继续")
	var questionID string
	m06Poll(t, 30*time.Second, "pending question", func() bool {
		for _, question := range m06Questions(t, ctx, env, sessionID) {
			if question.Status == sessionlog.QuestionPending {
				questionID = question.QuestionID
				return true
			}
		}
		return false
	})
	// Cancelling the run leaves the question pending in the session log.
	if err := stream.client.Cancel(sessionID, runID); err != nil {
		t.Fatal(err)
	}
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCancelled {
		t.Fatalf("cancelled run outcome = %+v", outcome)
	}
	stream.close()
	trace := m06ToolTraceOf(t, root, sessionID)
	if result := trace.resultOf(t, "ask_user"); result.Error != "" || result.Result != "Question cancelled" {
		t.Fatalf("cancelled ask result = %+v", result)
	}
	if questions := m06Questions(t, ctx, env, sessionID); len(questions) != 1 || questions[0].Status != sessionlog.QuestionPending {
		t.Fatalf("leftover questions = %+v", questions)
	}

	// The late reply still lands: with no waiting run it is queued as an
	// ordinary user message for the next run's context.
	const late = "就用方案B，继续写入"
	if _, err := conversation.Request(ctx, env.socket, conversation.ClientMsg{Op: "reply", SessionID: sessionID, QuestionID: questionID, Text: late}); err != nil {
		t.Fatalf("late reply failed: %v", err)
	}
	if questions := m06Questions(t, ctx, env, sessionID); len(questions) != 1 || questions[0].Status != sessionlog.QuestionReplied {
		t.Fatalf("questions after late reply = %+v", questions)
	}
	var queued bool
	for _, message := range m06MessageTexts(t, root, sessionID) {
		if message.Role == "user" && message.Text == late {
			queued = true
		}
	}
	if !queued {
		t.Fatalf("late reply was not queued as a user message: %+v", m06MessageTexts(t, root, sessionID))
	}

	// The next run's model context contains the queued text.
	provider.mu.Lock()
	provider.respond = func(call int, _ llm.Request) []llm.Event {
		return m06TextRound("已看到排队答复。")
	}
	provider.mu.Unlock()
	stream = m06StartRunWithText(t, ctx, env, sessionID, "run-after-cancel", "继续")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("follow-up run outcome = %+v", outcome)
	}
	found := false
	for _, message := range provider.roundMessages(2) {
		if strings.Contains(message.Content, late) {
			found = true
		}
	}
	if !found {
		t.Fatal("queued reply text is missing from the next run's model context")
	}
}

// ---------------------------------------------------------------------------
// Scenario d: todo tools — events, redaction, restart recovery.
// ---------------------------------------------------------------------------

func TestM06TodoPersistenceRedactionAndRestart(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	const credential = "m06-e2e-provider-secret"
	provider := &m06Agent{}
	env := m06NewService(t, root, db, provider, nil, credential, "")
	sessionID := m06SessionCreate(t, ctx, env)
	taskFile := filepath.Join(root, ".stable", "tasks", sessionID+".json")

	provider.mu.Lock()
	provider.respond = func(call int, req llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-create", "task_create", map[string]any{
				"subject": "审查走线", "description": "检查 token=" + credential + " 是否泄漏", "activeForm": "审查走线中",
			})
		case 2:
			// The create result names the generated task id.
			taskID := m06ExtractTaskID(t, req)
			return m06ToolRound("call-update", "task_update", map[string]any{"taskId": taskID, "status": "in_progress"})
		default:
			return m06TextRound("任务清单已更新。")
		}
	}
	provider.mu.Unlock()

	stream := m06StartRunWithText(t, ctx, env, sessionID, "run-todo-1", "维护任务清单")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("todo run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)

	var revisions []int
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventTodo) {
		var update sessionlog.TodoUpdate
		m06Decode(t, event.Data, &update)
		revisions = append(revisions, update.Revision)
		for _, snapshot := range update.Tasks {
			if strings.Contains(snapshot.Subject, credential) || strings.Contains(snapshot.Description, credential) {
				t.Fatalf("todo event leaks the credential: %+v", snapshot)
			}
		}
	}
	if len(revisions) != 2 || revisions[0] != 1 || revisions[1] != 2 {
		t.Fatalf("todo revisions = %v, want strictly increasing 1,2", revisions)
	}
	raw, err := os.ReadFile(taskFile)
	if err != nil {
		t.Fatalf("todo file missing: %v", err)
	}
	if strings.Contains(string(raw), credential) {
		t.Fatal("todo file leaks the credential")
	}
	if !strings.Contains(string(raw), "[credential redacted]") {
		t.Fatalf("todo file without redaction placeholder: %s", raw)
	}
	if info, err := os.Lstat(taskFile); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("todo file mode = %v, %v", info.Mode(), err)
	}
	trace := m06ToolTraceOf(t, root, sessionID)
	if result := trace.resultOf(t, "task_create"); result.Error != "" || !strings.Contains(m06ResultText(t, result), "Created task:") {
		t.Fatalf("task_create result = %+v", result)
	}
	if result := trace.resultOf(t, "task_update"); result.Error != "" || !strings.Contains(m06ResultText(t, result), "Updated task:") || !strings.Contains(m06ResultText(t, result), "in_progress") {
		t.Fatalf("task_update result = %+v", result)
	}

	// Restart the session service over the same project: the task list is
	// restored from disk, the journal continues at the next revision, and a
	// task_list call inside a fresh run sees the recovered state.
	env.svc.Close()
	env.cancel()
	provider2 := &m06Agent{}
	env = m06NewService(t, root, db, provider2, nil, credential, "")
	provider2.mu.Lock()
	provider2.respond = func(call int, req llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-list", "task_list", map[string]any{})
		case 2:
			taskID := m06ExtractTaskID(t, req)
			return m06ToolRound("call-complete", "task_update", map[string]any{"taskId": taskID, "status": "completed"})
		default:
			return m06TextRound("重启后清单恢复。")
		}
	}
	provider2.mu.Unlock()

	stream = m06StartRunWithText(t, ctx, env, sessionID, "run-todo-2", "重启后检查任务清单")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("post-restart todo run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)
	trace = m06ToolTraceOf(t, root, sessionID)
	listResult := trace.resultOf(t, "task_list")
	if listResult.Error != "" || !strings.Contains(m06ResultText(t, listResult), "[in_progress] 审查走线") {
		t.Fatalf("restored task_list result = %+v", listResult)
	}
	if result, ok := trace.results["call-complete"]; !ok || result.Error != "" || !strings.Contains(m06ResultText(t, result), "[completed]") {
		t.Fatalf("post-restart task_update result = %+v (present=%t)", result, ok)
	}
	revisions = nil
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventTodo) {
		var update sessionlog.TodoUpdate
		m06Decode(t, event.Data, &update)
		revisions = append(revisions, update.Revision)
	}
	if len(revisions) != 3 || revisions[2] != 3 {
		t.Fatalf("todo revisions across restart = %v, want 1,2,3", revisions)
	}
}

// m06ExtractTaskID pulls the generated task id out of the previous tool
// result so the scripted provider can address it in the next round.
func m06ExtractTaskID(t *testing.T, req llm.Request) string {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, part := range req.Messages[i].ToolResults {
			if match := regexp.MustCompile(`task-[0-9a-f]{16}`).FindString(part.Content); match != "" {
				return match
			}
		}
	}
	t.Fatal("no task id in the previous tool results")
	return ""
}

// ---------------------------------------------------------------------------
// Scenario e: goal proposal confirm/reject — protocol and state machine.
// ---------------------------------------------------------------------------

type m06Structured struct{}

func (m06Structured) Descriptor() core.ModelDescriptor {
	return core.ModelDescriptor{Provider: "fixture", Model: "fixture", Host: "fixture"}
}

func (m06Structured) GenerateStructured(_ context.Context, _ string, _ decision.SchemaID) (decision.StructuredOutput, error) {
	return decision.StructuredOutput{
		Data:              json.RawMessage(`{"status":"ok","criteria":[{"id":"c-1","kind":"kicad.erc_clean","payload":{"max_violations":0}}]}`),
		ProviderRequestID: "req-m06",
	}, nil
}

func TestM06ProposalProtocolStateMachines(t *testing.T) {
	ctx := context.Background()
	root, db := m06NewProject(t)
	env := m06NewService(t, root, db, &m06Agent{}, m06Structured{}, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO goals(id,objective,criteria_json,allowed_root,allowed_capabilities_json,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		"goal-m06", "协议层提案状态机", `[]`, root, `[]`, "active", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"ev-m06", "goal-m06", "c-1", "art-1", "manual", "pass", "report.md", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO agents(id,goal_id,status) VALUES(?,?,?)`, "agent-m06", "goal-m06", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO computer_sessions(id,goal_id,status) VALUES(?,?,?)`, "cs-m06", "goal-m06", "idle"); err != nil {
		t.Fatal(err)
	}

	// create_goal with a session: the criteria proposal is pushed to the
	// client, referenced by a session event, and stays pending.
	msgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "create_goal", ProjectRoot: env.root, SessionID: sessionID, Text: "修复传感器板走线并保持 ERC 干净"})
	var proposal *core.CriteriaProposal
	for _, m := range msgs {
		if m.Type == "proposal" {
			proposal = m.Proposal
		}
	}
	if proposal == nil || proposal.Status != core.ProposalPending || proposal.SessionID != sessionID || len(proposal.Criteria) != 1 {
		t.Fatalf("create_goal proposal = %+v", proposal)
	}
	var sessionProposal int
	for _, event := range m06EventsOfType(t, root, sessionID, sessionlog.EventProposal) {
		var stored core.CriteriaProposal
		m06Decode(t, event.Data, &stored)
		if stored.ID == proposal.ID {
			sessionProposal++
		}
	}
	if sessionProposal != 1 {
		t.Fatalf("session proposal references = %d", sessionProposal)
	}

	// Reject reaches the same state machine as the /reject text command.
	if _, err := conversation.Request(ctx, env.socket, conversation.ClientMsg{Op: "reject", SessionID: sessionID, ID: proposal.ID}); err != nil {
		t.Fatalf("reject failed: %v", err)
	}
	rejected, err := db.GetProposal(ctx, proposal.ID)
	if err != nil || rejected.Status != core.ProposalRejected {
		t.Fatalf("rejected proposal = %+v, %v", rejected, err)
	}

	// Confirm on a goal-attached proposal is the criteria-change state
	// machine: revision bump, pending re-verification, evidence invalidation.
	attached := core.CriteriaProposal{ID: "prop-m06-confirm", GoalID: "goal-m06", SessionID: sessionID,
		Status: core.ProposalPending, Criteria: []core.Criterion{{ID: "c-1", Kind: "kicad.erc_clean", Payload: json.RawMessage(`{"max_violations":0}`)}}, RawText: "升级后的标准"}
	if _, err := db.InsertProposal(ctx, attached); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventProposal, attached); err != nil {
		t.Fatal(err)
	}
	confirmMsgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "confirm", SessionID: sessionID, ID: attached.ID})
	var updatedGoal bool
	for _, m := range confirmMsgs {
		if m.Type == "goal_update" && m.Goal != nil && m.Goal.Status == core.GoalPendingReverification {
			updatedGoal = true
		}
	}
	if !updatedGoal {
		t.Fatalf("confirm messages = %s", m06Dump(confirmMsgs))
	}
	confirmed, err := db.GetProposal(ctx, attached.ID)
	if err != nil || confirmed.Status != core.ProposalConfirmed {
		t.Fatalf("confirmed proposal = %+v, %v", confirmed, err)
	}
	snapshot, err := db.GetGoalSnapshot(ctx, "goal-m06")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.CriteriaRevision != 1 {
		t.Fatalf("goal after confirm = %+v", snapshot.Goal)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].InvalidatedReason == "" {
		t.Fatalf("evidence after confirm = %+v", snapshot.Evidence)
	}
	var confirmMessage bool
	for _, m := range confirmMsgs {
		if m.Type == "message" && m.Message != nil && m.Message.Kind == core.MessageKindCriteriaConfirm {
			confirmMessage = true
		}
	}
	if !confirmMessage {
		t.Fatalf("confirm did not produce the criteria-confirm message: %s", m06Dump(confirmMsgs))
	}
}
