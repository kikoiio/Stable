package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// writeSkillDir creates one skill directory with the given files.
func writeSkillDir(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// newSkillFixture builds a project with one session and a skill gate bound to
// it. The skill directories start with the given fixtures and stay writable
// so the tests can exercise the delta path.
func newSkillFixture(t *testing.T, userFiles, projectFiles map[string]map[string]string) (*Service, *SkillGate, string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	userDir := t.TempDir()
	projectDir := t.TempDir()
	for name, files := range userFiles {
		writeSkillDir(t, userDir, name, files)
	}
	for name, files := range projectFiles {
		writeSkillDir(t, projectDir, name, files)
	}
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gate := NewSkillGate(nil, userDir, projectDir)
	svc := &Service{deps: Deps{Store: db, ProjectRoot: root, Skills: gate, ProviderName: "fixture", Model: "fixture"}, clients: map[chan ServerMsg]*clientSubscription{}}
	gate.Bind(svc)
	return svc, gate, root, session.ID, userDir, projectDir
}

func skillEvents(t *testing.T, root, sessionID string) []sessionlog.Event {
	t.Helper()
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []sessionlog.Event
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventSkillInventory, sessionlog.EventSkillDelta, sessionlog.EventSkillInvoked:
			out = append(out, event)
		}
	}
	return out
}

const inlineSkillMD = "---\nname: code-review\ndescription: 审查当前改动\nwhen_to_use: 提交前\n---\n\n1. 逐文件审查 $ARGUMENTS\n2. 输出结论\n"

// Activating through the slash entry renders the body with the M06 argument
// semantics, journals one skill_invoked event and records the activation.
func TestSkillGateActivateSlashJournalsAndRenders(t *testing.T) {
	_, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
	})
	body, err := gate.activate(sessionID, "code-review", "性能", sessionlog.SkillEntrySlash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "1. 逐文件审查 性能") {
		t.Fatalf("rendered body = %q", body)
	}
	events := skillEvents(t, root, sessionID)
	if len(events) != 1 || events[0].Type != sessionlog.EventSkillInvoked {
		t.Fatalf("skill events = %+v", events)
	}
	var invoked sessionlog.SkillInvoked
	if err := decodeSessionData(events[0].Data, &invoked); err != nil {
		t.Fatal(err)
	}
	if invoked.Name != "code-review" || invoked.Source != "project" || invoked.Entry != sessionlog.SkillEntrySlash || invoked.Args != "性能" {
		t.Fatalf("invoked = %+v", invoked)
	}
	_, activated := gate.List(sessionID)
	if len(activated) != 1 || activated[0] != "code-review" {
		t.Fatalf("activated = %v", activated)
	}
}

// Unknown skills list the available names; fork-mode skills are refused
// until M09; neither produces an activation record.
func TestSkillGateUnknownAndForkRefused(t *testing.T) {
	_, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
		"deep-dive":   {"SKILL.md": "---\nname: deep-dive\nmode: fork\n---\n\n隔离步骤\n"},
	})
	if _, err := gate.activate(sessionID, "nope", "", sessionlog.SkillEntrySlash); err == nil || !strings.Contains(err.Error(), "unknown skill: nope") || !strings.Contains(err.Error(), "code-review") {
		t.Fatalf("unknown skill error = %v", err)
	}
	if _, err := gate.activate(sessionID, "deep-dive", "", sessionlog.SkillEntrySlash); err == nil || !strings.Contains(err.Error(), "子 agent 能力未启用") {
		t.Fatalf("fork error = %v", err)
	}
	if _, err := gate.LoadSkill(context.Background(), sessionID, "deep-dive", ""); err == nil || !strings.Contains(err.Error(), "fork 模式留待 M09") {
		t.Fatalf("tool-entry fork error = %v", err)
	}
	if events := skillEvents(t, root, sessionID); len(events) != 0 {
		t.Fatalf("failed activations must not journal, events = %+v", events)
	}
	infos, _ := gate.List(sessionID)
	if len(infos) != 2 {
		t.Fatalf("fork skill must stay listable, infos = %+v", infos)
	}
}

// The inventory is journaled once; skills added mid-session surface as a
// one-shot delta and never repeat.
func TestSkillGateInventoryAndDeltaOnce(t *testing.T) {
	_, gate, root, sessionID, _, projectDir := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
	})
	snapshot, delta, err := gate.SkillInventory(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, "code-review") || !strings.Contains(snapshot, "审查当前改动") || !strings.Contains(snapshot, "适用场景: 提交前") {
		t.Fatalf("snapshot text = %q", snapshot)
	}
	if delta != "" {
		t.Fatalf("first inventory delta = %q", delta)
	}
	writeSkillDir(t, projectDir, "perf-tips", map[string]string{
		"SKILL.md": "---\nname: perf-tips\ndescription: 性能优化清单\n---\n\n先测量\n",
	})
	snapshot, delta, err = gate.SkillInventory(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delta, "perf-tips") {
		t.Fatalf("delta = %q", delta)
	}
	if !strings.Contains(snapshot, "perf-tips") {
		t.Fatalf("snapshot after delta = %q", snapshot)
	}
	_, delta, err = gate.SkillInventory(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if delta != "" {
		t.Fatalf("delta repeated: %q", delta)
	}
	var inventories, deltas int
	for _, event := range skillEvents(t, root, sessionID) {
		switch event.Type {
		case sessionlog.EventSkillInventory:
			inventories++
		case sessionlog.EventSkillDelta:
			deltas++
		}
	}
	if inventories != 1 || deltas != 1 {
		t.Fatalf("inventories = %d, deltas = %d, want 1/1", inventories, deltas)
	}
}

// A restarted service rebuilds snapshot, notified names and activations from
// the log: no duplicate inventory (the log would refuse it), no repeated
// delta, and the activation list survives.
func TestSkillGateRestoreAfterRestart(t *testing.T) {
	svc, gate, root, sessionID, userDir, projectDir := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
	})
	if _, _, err := gate.SkillInventory(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.activate(sessionID, "code-review", "", sessionlog.SkillEntryTool); err != nil {
		t.Fatal(err)
	}
	writeSkillDir(t, projectDir, "perf-tips", map[string]string{
		"SKILL.md": "---\nname: perf-tips\ndescription: 性能优化清单\n---\n\n先测量\n",
	})

	restored := NewSkillGate(nil, userDir, projectDir)
	restored.Bind(svc)
	infos, activated := restored.List(sessionID)
	if len(activated) != 1 || activated[0] != "code-review" {
		t.Fatalf("restored activations = %v", activated)
	}
	snapshot, delta, err := restored.SkillInventory(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("restored inventory must not duplicate the journaled snapshot: %v", err)
	}
	// Skills added while the service was down are announced once on the
	// first run after restart.
	if !strings.Contains(snapshot, "perf-tips") || !strings.Contains(delta, "perf-tips") {
		t.Fatalf("restored snapshot = %q, delta = %q", snapshot, delta)
	}
	if len(infos) != 2 {
		t.Fatalf("restored infos = %+v", infos)
	}
	inventories := 0
	for _, event := range skillEvents(t, root, sessionID) {
		if event.Type == sessionlog.EventSkillInventory {
			inventories++
		}
	}
	if inventories != 1 {
		t.Fatalf("inventories = %d, want 1", inventories)
	}
}

// The skill_reload op rescans and reports counts; skills added since the
// last run surface as a delta on the next run.
func TestSkillReloadOpAndForcedDiff(t *testing.T) {
	svc, gate, _, sessionID, _, projectDir := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
	})
	if _, _, err := gate.SkillInventory(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	writeSkillDir(t, projectDir, "perf-tips", map[string]string{
		"SKILL.md": "---\nname: perf-tips\ndescription: 性能优化清单\n---\n\n先测量\n",
	})
	msgs, err := svc.reloadSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Type != "skill_report" || msgs[0].SkillReport == nil {
		t.Fatalf("reload response = %+v", msgs)
	}
	report := *msgs[0].SkillReport
	if report.Kind != SkillReportReload || report.Before != 1 || report.After != 2 {
		t.Fatalf("reload report = %+v", report)
	}
	_, delta, err := gate.SkillInventory(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delta, "perf-tips") {
		t.Fatalf("post-reload delta = %q", delta)
	}
}

// skill_invoke activates through the slash entry and starts a run whose
// message list carries the rendered body; failures report through the skill
// report shape and start no run.
func TestSkillInvokeOpStartsRunWithBody(t *testing.T) {
	svc, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
		"deep-dive":   {"SKILL.md": "---\nname: deep-dive\nmode: fork\n---\n\n隔离步骤\n"},
	})
	updates := make(chan ServerMsg, 8)
	svc.mu.Lock()
	svc.clients[updates] = &clientSubscription{ch: updates}
	svc.mu.Unlock()

	if err := svc.invokeSkill(context.Background(), ClientMsg{Op: "skill_invoke", SessionID: sessionID, SkillName: "deep-dive"}, updates); err == nil || !strings.Contains(err.Error(), "子 agent 能力未启用") {
		t.Fatalf("fork invoke error = %v", err)
	}

	runner := newTerminalRunner(sessionID, "run-skill")
	svc.deps.Runner = runner
	if err := svc.invokeSkill(context.Background(), ClientMsg{Op: "skill_invoke", SessionID: sessionID, SkillName: "code-review", SkillArgs: "聚焦内存"}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages := runner.request.Messages
	if len(messages) != 3 {
		t.Fatalf("skill run messages = %+v", messages)
	}
	if messages[0].Role != "system" {
		t.Fatalf("first message = %+v", messages[0])
	}
	if !strings.Contains(messages[1].Content, "code-review") {
		t.Fatalf("skill snapshot not injected: %q", messages[1].Content)
	}
	if !strings.Contains(messages[2].Content, "1. 逐文件审查 聚焦内存") {
		t.Fatalf("rendered body not in run messages: %+v", messages[2])
	}
	// The rendered body is history; the skill events are metadata.
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(transcript.Events)
	if !strings.Contains(string(encoded), "逐文件审查 聚焦内存") {
		t.Fatal("rendered body missing from the session log")
	}
	_ = gate
}

// The inventory rides per-run context after the system prefix and before the
// history; it never lands in the session log and keeps injecting after a
// compaction boundary replaced the earlier history.
func TestStartRunSkillInjectionPlacementAndPersistence(t *testing.T) {
	svc, gate, root, sessionID, _, _ := newSkillFixture(t, nil, map[string]map[string]string{
		"code-review": {"SKILL.md": inlineSkillMD},
	})
	updates := make(chan ServerMsg, 8)
	svc.mu.Lock()
	svc.clients[updates] = &clientSubscription{ch: updates}
	svc.mu.Unlock()

	runner := newTerminalRunner(sessionID, "run-1")
	svc.deps.Runner = runner
	first := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "开工", Messages: []llm.Message{{Role: "user", Content: "开工"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: sessionID, Run: &first}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages := runner.request.Messages
	if len(messages) != 3 || messages[0].Role != "system" || !strings.Contains(messages[1].Content, "code-review") || messages[2].Content != "开工" {
		t.Fatalf("skill run messages = %+v", messages)
	}
	// Activating the skill adds the activated note to the inventory text.
	if _, err := gate.activate(sessionID, "code-review", "", sessionlog.SkillEntrySlash); err != nil {
		t.Fatal(err)
	}
	// A compaction boundary replaces the earlier history; the inventory text
	// must still ride on the next run with the activated-skill note. Run-scope
	// boundaries use run-scoped seqs: run-1 carried exactly one event
	// (RunSeq 1, the terminal).
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventBoundary, sessionlog.Boundary{FromSeq: 1, ToSeq: 1, Summary: "更早的讨论摘要", Scope: sessionlog.BoundaryScopeRun, RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	runner = newTerminalRunner(sessionID, "run-2")
	svc.deps.Runner = runner
	second := agent.ExecutionRequest{RunID: "run-2", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "继续", Messages: []llm.Message{{Role: "user", Content: "继续"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: sessionID, Run: &second}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages = runner.request.Messages
	if len(messages) < 3 || !strings.Contains(messages[1].Content, "已激活技能") || !strings.Contains(messages[1].Content, "code-review") {
		t.Fatalf("post-compaction skill injection = %+v", messages)
	}
	// The inventory text is context, not history.
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(transcript.Events)
	if strings.Contains(string(encoded), "可用技能") || strings.Contains(string(encoded), "已激活技能") {
		t.Fatal("skill inventory text leaked into the session log")
	}
}
