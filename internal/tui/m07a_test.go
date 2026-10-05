package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/conversation"
	"stable/internal/sessionlog"
)

// writeSkillDirMD creates one SKILL.md skill under the given skills root.
func writeSkillDirMD(t *testing.T, skillsRoot, name, content string) {
	t.Helper()
	dir := filepath.Join(skillsRoot, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// isolateSkillDirs points XDG_CONFIG_HOME and HOME at fresh temp dirs so the
// user-level skill directory is test-local.
func isolateSkillDirs(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	return filepath.Join(xdg, "stable", "skills")
}

// --- T11 冲突优先级：内置 > 命令文件 > 技能；补全可见 ---

func TestSkillCommandsCompleteBelowCommandsAndBuiltins(t *testing.T) {
	userSkills := isolateSkillDirs(t)
	root := t.TempDir()
	writeSkillDirMD(t, userSkills, "alpha", "---\nname: alpha\ndescription: 用户技能\n---\nAlpha body\n")
	writeSkillDirMD(t, userSkills, "search", "---\nname: search\ndescription: 技能搜索\n---\nSkill search body\n")
	writeSkillDirMD(t, filepath.Join(root, ".stable", "skills"), "beta", "---\nname: beta\ndescription: 项目技能\n---\nBeta body\n")
	writeCommandFile(t, root, "alpha.md",
		"---\ndescription: 自定义 alpha\n---\n命令正文\n")

	m := New("sock", root)
	m.Composer.SetValue("/")
	m.refreshCompletions()

	var alpha, search, beta *CompletionItem
	for i := range m.Candidates {
		switch m.Candidates[i].Label {
		case "/alpha":
			alpha = &m.Candidates[i]
		case "/search":
			search = &m.Candidates[i]
		case "/beta":
			beta = &m.Candidates[i]
		}
	}
	if alpha == nil || alpha.Detail != "自定义 alpha" {
		t.Fatalf("command file must shadow the skill command: %+v", alpha)
	}
	if search == nil || search.Detail != "搜索会话内容" {
		t.Fatalf("builtin must shadow the skill command: %+v", search)
	}
	if beta == nil || !strings.HasPrefix(beta.Detail, "（技能）") || !strings.Contains(beta.Detail, "项目技能") {
		t.Fatalf("skill command missing or unmarked: %+v", beta)
	}
}

// --- T11 技能命令经 skill_invoke op 激活 ---

func TestSkillDispatchSendsInvokeOp(t *testing.T) {
	isolateSkillDirs(t)
	root := t.TempDir()
	writeSkillDirMD(t, filepath.Join(root, ".stable", "skills"), "beta", "---\nname: beta\ndescription: 项目技能\n---\nBeta body\n")

	m := New("sock", root)
	m.ActiveSession = "s1"
	m.Composer.SetValue("/beta 审查重点")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	if cmd == nil || !got.Pending {
		t.Fatalf("skill command did not submit: cmd=%v", cmd)
	}
	if got.Composer.Value() != "" {
		t.Fatalf("composer not cleared: %q", got.Composer.Value())
	}
	// The skill command opens a run stream, exactly like the chat path.
	if _, ok := cmd().(runStreamStartedMsg); !ok {
		t.Fatalf("expected run stream command, got %T", cmd())
	}
	entries, err := got.history.List()
	if err != nil || len(entries) != 1 || entries[0].Text != "/beta 审查重点" {
		t.Fatalf("history should record the typed command: %+v err=%v", entries, err)
	}
}

// --- T11 /skills 列表、reload 与已激活查询 ---

func TestSkillsCommandListsAndReloadSendsOps(t *testing.T) {
	isolateSkillDirs(t)
	root := t.TempDir()
	writeSkillDirMD(t, filepath.Join(root, ".stable", "skills"), "beta", "---\nname: beta\ndescription: 项目技能\n---\nBeta body\n")

	m := New("sock", root)
	m.ActiveSession = "s1"
	m.Composer.SetValue("/skills")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	var last sessionlog.Message
	b, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	if json.Unmarshal(b, &last) != nil || !strings.Contains(last.Text, "可用技能") || !strings.Contains(last.Text, "/beta — 项目技能") {
		t.Fatalf("/skills listing = %q", last.Text)
	}
	if cmd == nil {
		t.Fatal("/skills should query the activated list for the active session")
	}
	if msg, ok := cmd().(resultMsg); !ok || msg.op != "skill_list" {
		t.Fatalf("expected skill_list op, got %T", cmd())
	}

	m2 := New("sock", root)
	m2.ActiveSession = "s1"
	m2.Composer.SetValue("/skills reload")
	updated2, cmd2 := m2.submitComposer()
	got2 := updated2.(Model)
	if !strings.Contains(got2.Status, "技能已重载：1 → 1") {
		t.Fatalf("reload status = %q", got2.Status)
	}
	if msg, ok := cmd2().(resultMsg); !ok || msg.op != "skill_reload" {
		t.Fatalf("expected skill_reload op, got %T", cmd2())
	}
}

// --- T11/T12 SkillReport 反馈与技能事件渲染 ---

func TestSkillReportStatusAndTranscriptRendering(t *testing.T) {
	isolateSkillDirs(t)
	m := New("sock", t.TempDir())
	updated, _ := m.handleResult(resultMsg{op: "skill_invoke", msgs: []conversation.ServerMsg{{
		Type:        "skill_report",
		SkillReport: &conversation.SkillReport{Kind: conversation.SkillReportError, Name: "deep-dive", Error: "子 agent 能力未启用(fork 模式留待 M09): deep-dive"},
	}}})
	got := updated.(Model)
	if !strings.Contains(got.Status, "技能调用失败") || !strings.Contains(got.Status, "deep-dive") {
		t.Fatalf("error report status = %q", got.Status)
	}
	updated, _ = got.handleResult(resultMsg{op: "skill_reload", msgs: []conversation.ServerMsg{{
		Type:        "skill_report",
		SkillReport: &conversation.SkillReport{Kind: conversation.SkillReportReload, Before: 1, After: 2},
	}}})
	got = updated.(Model)
	if !strings.Contains(got.Status, "服务端技能已重载：1 → 2") {
		t.Fatalf("reload report status = %q", got.Status)
	}
	updated, _ = got.handleResult(resultMsg{op: "run_subscribe", msgs: []conversation.ServerMsg{{
		Type:        "skill_report",
		SkillReport: &conversation.SkillReport{Kind: conversation.SkillReportDelta, Added: []string{"perf-tips"}},
	}}})
	got = updated.(Model)
	if !strings.Contains(got.Status, "新增可用技能：perf-tips") {
		t.Fatalf("delta report status = %q", got.Status)
	}

	events := []sessionlog.Event{
		{Type: sessionlog.EventSkillInventory, Data: sessionlog.SkillInventory{Skills: []sessionlog.SkillInfo{{Name: "beta", Source: "project"}}}},
		{Type: sessionlog.EventSkillDelta, Data: sessionlog.SkillDelta{Added: []sessionlog.SkillInfo{{Name: "perf-tips", Source: "project"}}}},
		{Type: sessionlog.EventSkillInvoked, Data: sessionlog.SkillInvoked{Name: "beta", Source: "project", Entry: "slash"}},
	}
	out := projectTranscript(events, 80, false)
	for _, want := range []string{"技能清单", "beta（project）", "技能新增", "perf-tips", "技能激活", "beta（project·slash）"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in transcript:\n%s", want, out)
		}
	}
}
