package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/commands"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/skills"
	"stable/internal/store"
	"stable/internal/tui"
)

// ---------------------------------------------------------------------------
// M07-A harness: the M06 service wiring plus the skill gate, the load_skill
// tool schema and the SkillProvider injection.
// ---------------------------------------------------------------------------

func m07aToolSchemas() []llm.ToolSchema {
	schemas := m06ToolSchemas()
	for _, schema := range execution.SkillToolSchemas() {
		name, _ := schema["name"].(string)
		description, _ := schema["description"].(string)
		input, _ := schema["input_schema"].(map[string]any)
		schemas = append(schemas, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

func m07aNewService(t *testing.T, root string, db *store.Store, provider *m06Agent, credential, helperPath string) *m06Env {
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
	// The skill gate reads the same directories the TUI lists; Serve binds it
	// so its event appends share the service event mutex.
	skillGate := conversation.NewSkillGate(nil, os.Getenv("STABLE_M07A_USER_SKILLS"), filepath.Join(root, ".stable", "skills"))
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
	}, execution.WithPlanSink(planSink), execution.WithSkillProvider(skillGate))
	runner := agent.NewRunner(provider, agent.RunnerOptions{ExecutorFactory: factory, ToolSchemas: m07aToolSchemas(), MaxRetries: -1})
	socket := filepath.Join(filepath.Dir(root), fmt.Sprintf("chat-%d.sock", time.Now().UnixNano()))
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, Provider: nil, ChatProvider: provider, Runner: runner,
		ProviderCredential: credential, ProviderName: "fixture", Model: "fixture",
		ProjectRoot: root, SocketPath: socket, PollEvery: 200 * time.Millisecond,
		Snapshots: snapshots,
		Skills:    skillGate,
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

// m07aWriteSkill creates a SKILL.md skill under dir; m07aWriteYamlSkill
// creates the legacy skill.yaml + prompt.md layout.
func m07aWriteSkill(t *testing.T, dir, name, frontmatter, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\n"+frontmatter+"---\n\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

func m07aWriteYamlSkill(t *testing.T, dir, name, yamlMeta, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(yamlMeta), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "prompt.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

// m07aInvokeSkill starts a skill run through the real skill_invoke op and
// collects the stream until the outcome (or the error termination).
func m07aInvokeSkill(t *testing.T, ctx context.Context, env *m06Env, sessionID, name, args string) *m06RunStream {
	t.Helper()
	client, err := conversation.InvokeSkill(ctx, env.socket, sessionID, name, args)
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
			if message.Type == "done" {
				return
			}
		}
	}()
	return stream
}

// m07aRoundSnapshot returns the inventory injection text the fake agent saw
// in the given round — the stable list message, or "" when absent.
func m07aRoundSnapshot(t *testing.T, p *m06Agent, call int) string {
	t.Helper()
	messages := p.roundMessages(call)
	for _, m := range messages {
		if m.Role == "user" && strings.Contains(m.Content, "可用技能") {
			return m.Content
		}
	}
	return ""
}

// m07aRoundReminder reports whether the round carried the one-shot delta
// reminder, which rides as its own message after the inventory text.
func m07aRoundReminder(t *testing.T, p *m06Agent, call int) string {
	t.Helper()
	messages := p.roundMessages(call)
	for _, m := range messages {
		if m.Role == "user" && strings.Contains(m.Content, "新增可用") {
			return m.Content
		}
	}
	return ""
}

// m07aUserSkillDir returns an isolated user-level skill directory and points
// the service-side gate env at it.
func m07aUserSkillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STABLE_M07A_USER_SKILLS", dir)
	return dir
}

// The full skill loop: both layouts load, the slash command activates through
// skill_invoke and lands the rendered body as a session message, the agent
// inventory text lists the skills, load_skill activates from the tool side,
// fork skills are refused through both entries, bodies hot-reload without a
// restart, and the reload op reports the skill counts.
func TestM07ASkillActivationHotReloadAndForkRefusal(t *testing.T) {
	ctx := context.Background()
	userDir := m07aUserSkillDir(t)
	root, db := m06NewProject(t)
	projectDir := filepath.Join(root, ".stable", "skills")

	// Layout ① with an $ARGUMENTS placeholder, layout ② (skill.yaml), and a
	// fork-mode skill that must stay visible but refuse activation.
	m07aWriteSkill(t, projectDir, "code-review",
		"name: code-review\ndescription: 审查当前改动\nwhen_to_use: 提交前\n",
		"1. 逐文件审查 $ARGUMENTS\n2. 输出结论\n")
	m07aWriteYamlSkill(t, userDir, "user-note", "name: user-note\ndescription: 用户速记\n", "把要点记入会议纪要。\n")
	m07aWriteSkill(t, projectDir, "deep-dive", "name: deep-dive\nmode: fork\n", "隔离步骤\n")

	// TUI-side replication: catalog, slash command registration and the
	// completion list come from the same building blocks the Model wires.
	catalog := skills.LoadCatalog(userDir, projectDir)
	list := catalog.List()
	if len(list) != 3 {
		t.Fatalf("catalog = %+v, want 3 skills", list)
	}
	for _, skill := range list {
		if skill.Source != "project" && skill.Source != "user" {
			t.Fatalf("skill %q source = %q", skill.Meta.Name, skill.Source)
		}
	}
	registry := commands.NewRegistry()
	for _, skill := range list {
		if !registry.RegisterOptional(&commands.Command{Name: skill.Meta.Name, Description: "（技能）" + skill.Meta.Description, Kind: commands.KindLocal}) {
			t.Fatalf("skill %q collided on an empty registry", skill.Meta.Name)
		}
	}
	if _, ok := registry.Find("code-review"); !ok {
		t.Fatal("skill command missing from the registry")
	}
	var seen bool
	for _, item := range tui.CommandItems(registry.List()) {
		if item.Label == "/user-note" && strings.HasPrefix(item.Detail, "（技能）") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("user-level layout-② skill missing from completion")
	}

	env := m07aNewService(t, root, db, &m06Agent{}, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	// This fixture has no child runner configured, so fork invocation must
	// report the unavailable service and avoid recording a partial activation.
	forkStream := m07aInvokeSkill(t, ctx, env, sessionID, "deep-dive", "")
	deadline := time.Now().Add(15 * time.Second)
	var forkErr string
	for time.Now().Before(deadline) {
		for _, m := range forkStream.messages() {
			if m.Type == "error" {
				forkErr = m.Error
			}
		}
		if forkErr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	forkStream.close()
	if !strings.Contains(forkErr, "fork skill execution is unavailable") {
		t.Fatalf("fork skill invocation error = %q", forkErr)
	}
	if texts := m06MessageTexts(t, root, sessionID); len(texts) != 0 {
		t.Fatalf("fork refusal produced session messages: %+v", texts)
	}

	// Slash activation: the rendered body enters the session as the run's
	// user message and the agent sees the skill inventory text.
	stream := m07aInvokeSkill(t, ctx, env, sessionID, "code-review", "走线宽度")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("skill run outcome = %+v", outcome)
	}
	stream.assertNoErrors(t)
	texts := m06MessageTexts(t, root, sessionID)
	if len(texts) == 0 || texts[0].Text != "1. 逐文件审查 走线宽度\n2. 输出结论" {
		t.Fatalf("session messages = %+v, want the rendered skill body first", texts)
	}
	invokedEvents := m06EventsOfType(t, root, sessionID, "skill_invoked")
	if len(invokedEvents) != 1 {
		t.Fatalf("skill_invoked events = %d, want 1", len(invokedEvents))
	}
	var invoked sessionlog.SkillInvoked
	m06Decode(t, invokedEvents[0].Data, &invoked)
	if invoked.Name != "code-review" || invoked.Entry != sessionlog.SkillEntrySlash || invoked.Args != "走线宽度" {
		t.Fatalf("invoked = %+v", invoked)
	}
	if snapshot := m07aRoundSnapshot(t, env.agent, 1); !strings.Contains(snapshot, "code-review") || !strings.Contains(snapshot, "user-note") {
		t.Fatalf("agent inventory text = %q", snapshot)
	}

	// Tool activation: the agent calls load_skill; the result carries the
	// header plus the freshest body, and the entry is journaled as "tool".
	env.agent.reset()
	env.agent.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-ls-1", "load_skill", map[string]any{"name": "code-review", "args": "性能"})
		default:
			return m06TextRound("技能已加载。")
		}
	}
	runStream := m06StartRun(t, ctx, env, sessionID, "run-tool-1")
	if outcome := runStream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("tool run outcome = %+v", outcome)
	}
	trace := m06ToolTraceOf(t, root, sessionID)
	result := trace.resultOf(t, "load_skill")
	resultText := m06ResultText(t, result)
	if !strings.Contains(resultText, "# Skill: code-review") || !strings.Contains(resultText, "1. 逐文件审查 性能") {
		t.Fatalf("load_skill result = %q", result)
	}
	invokedEvents = m06EventsOfType(t, root, sessionID, "skill_invoked")
	if len(invokedEvents) != 2 {
		t.Fatalf("skill_invoked events after tool call = %d, want 2", len(invokedEvents))
	}

	// Body-level hot reload: rewrite the file, the next load_skill returns
	// the new body without any restart.
	m07aWriteSkill(t, projectDir, "code-review",
		"name: code-review\ndescription: 审查当前改动\nwhen_to_use: 提交前\n",
		"1. 逐文件审查 $ARGUMENTS\n2. 输出结论\n3. 复核修复\n")
	env.agent.reset()
	env.agent.respond = func(call int, _ llm.Request) []llm.Event {
		switch call {
		case 1:
			return m06ToolRound("call-ls-2", "load_skill", map[string]any{"name": "code-review"})
		default:
			return m06TextRound("技能已重新加载。")
		}
	}
	runStream = m06StartRun(t, ctx, env, sessionID, "run-tool-2")
	if outcome := runStream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("hot reload run outcome = %+v", outcome)
	}
	// resultOf returns the first match; the hot-reload assertion needs the
	// latest load_skill result of the session.
	hotTrace := m06ToolTraceOf(t, root, sessionID)
	var lastLoadSkill string
	for _, callID := range hotTrace.order {
		if hotTrace.calls[callID].Name == "load_skill" {
			lastLoadSkill = m06ResultText(t, hotTrace.results[callID])
		}
	}
	if !strings.Contains(lastLoadSkill, "3. 复核修复") {
		t.Fatalf("hot body reload missed the new text: %q", lastLoadSkill)
	}

	// The reload op rescans the directories and reports the counts.
	reloadMsgs := m06Op(t, ctx, env.socket, conversation.ClientMsg{Op: "skill_reload"})
	var reloadReport *conversation.SkillReport
	for _, m := range reloadMsgs {
		if m.Type == "skill_report" && m.SkillReport != nil && m.SkillReport.Kind == conversation.SkillReportReload {
			report := *m.SkillReport
			reloadReport = &report
		}
	}
	if reloadReport == nil || reloadReport.Before != 3 || reloadReport.After != 3 {
		t.Fatalf("reload report = %+v, want 3 → 3", reloadReport)
	}
}

// The inventory snapshot rides the first run, mid-session additions surface
// exactly once as a delta, and a restart rebuilds snapshot, notified names
// and activations from the log without duplicating events.
func TestM07ASkillInventoryDeltaAndRestartConsistency(t *testing.T) {
	ctx := context.Background()
	m07aUserSkillDir(t)
	root, db := m06NewProject(t)
	projectDir := filepath.Join(root, ".stable", "skills")
	m07aWriteSkill(t, projectDir, "alpha", "name: alpha\ndescription: 基线技能\n", "基线步骤\n")

	provider := &m06Agent{}
	env := m07aNewService(t, root, db, provider, "", "")
	sessionID := m06SessionCreate(t, ctx, env)

	// Run 1: the agent sees the stable inventory snapshot; the snapshot event
	// is journaled once.
	stream := m06StartRun(t, ctx, env, sessionID, "run-inv-1")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("run 1 outcome = %+v", outcome)
	}
	if snapshot := m07aRoundSnapshot(t, provider, 1); !strings.Contains(snapshot, "alpha") || !strings.Contains(snapshot, "基线技能") {
		t.Fatalf("run 1 inventory = %q", snapshot)
	}
	if got := len(m06EventsOfType(t, root, sessionID, "skill_inventory")); got != 1 {
		t.Fatalf("skill_inventory events = %d, want 1", got)
	}

	// Activate alpha so the activation list survives the later restart.
	forkFree := m07aInvokeSkill(t, ctx, env, sessionID, "alpha", "")
	if outcome := forkFree.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("alpha activation outcome = %+v", outcome)
	}

	// Run 2 after a mid-session addition: the delta reminder rides once.
	m07aWriteSkill(t, projectDir, "beta", "name: beta\ndescription: 新增技能\n", "新增步骤\n")
	provider.reset()
	stream = m06StartRun(t, ctx, env, sessionID, "run-inv-2")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("run 2 outcome = %+v", outcome)
	}
	snapshot := m07aRoundSnapshot(t, provider, 1)
	if !strings.Contains(snapshot, "beta") {
		t.Fatalf("run 2 snapshot = %q, want beta in the inventory", snapshot)
	}
	if reminder := m07aRoundReminder(t, provider, 1); !strings.Contains(reminder, "beta") {
		t.Fatalf("run 2 delta reminder = %q", reminder)
	}
	if got := len(m06EventsOfType(t, root, sessionID, "skill_delta")); got != 1 {
		t.Fatalf("skill_delta events = %d, want 1", got)
	}

	// Run 3: no repeated delta for the same skill.
	provider.reset()
	stream = m06StartRun(t, ctx, env, sessionID, "run-inv-3")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("run 3 outcome = %+v", outcome)
	}
	if snapshot := m07aRoundSnapshot(t, provider, 1); strings.Contains(snapshot, "新增可用") {
		t.Fatalf("run 3 repeated the delta: %q", snapshot)
	}
	if reminder := m07aRoundReminder(t, provider, 1); reminder != "" {
		t.Fatalf("run 3 repeated the delta reminder: %q", reminder)
	}
	if got := len(m06EventsOfType(t, root, sessionID, "skill_delta")); got != 1 {
		t.Fatalf("skill_delta events after run 3 = %d, want 1", got)
	}

	// Restart: the same session keeps its inventory (no duplicate snapshot —
	// the journal would refuse it), announces nothing again, and still lists
	// the activated skill.
	env.svc.Close()
	env.cancel()
	provider.reset()
	restarted := m07aNewService(t, root, db, provider, "", "")
	stream = m06StartRun(t, ctx, restarted, sessionID, "run-inv-4")
	if outcome := stream.waitOutcome(t, 60*time.Second); outcome.Status != agent.RunCompleted {
		t.Fatalf("run 4 outcome = %+v", outcome)
	}
	snapshot = m07aRoundSnapshot(t, provider, 1)
	if !strings.Contains(snapshot, "alpha") || !strings.Contains(snapshot, "beta") || !strings.Contains(snapshot, "已激活技能") {
		t.Fatalf("post-restart snapshot = %q", snapshot)
	}
	if got := len(m06EventsOfType(t, root, sessionID, "skill_inventory")); got != 1 {
		t.Fatalf("skill_inventory events after restart = %d, want 1", got)
	}
	if got := len(m06EventsOfType(t, root, sessionID, "skill_delta")); got != 1 {
		t.Fatalf("skill_delta events after restart = %d, want 1", got)
	}

	// The projection replays the three event kinds for the transcript, and
	// the transcript round-trips through a fresh replay.
	transcript := m06Transcript(t, root, sessionID)
	kinds := map[string]int{}
	for _, event := range transcript.Events {
		switch event.Type {
		case "skill_inventory", "skill_delta", "skill_invoked":
			kinds[event.Type]++
		}
	}
	b, _ := json.Marshal(kinds)
	if !strings.Contains(string(b), "skill_invoked") {
		t.Fatalf("transcript kinds = %s", b)
	}
}
