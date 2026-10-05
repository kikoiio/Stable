package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/sessionlog"
)

// writeCommandFile creates one custom command file in the project commands
// directory of root.
func writeCommandFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, ".stable", "commands", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// --- T12.1 补全来自注册表：自定义命令并入，内置同名优先 ---

func TestCompletionMergesCustomCommandsAndKeepsBuiltins(t *testing.T) {
	// 隔离用户级命令目录，让断言只取决于项目命令。
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeCommandFile(t, root, "git/log.md",
		"---\ndescription: 展示提交历史\nargument-hint: '<范围>'\n---\n请总结 $ARGUMENTS\n")
	writeCommandFile(t, root, "search.md",
		"---\ndescription: 自定义搜索\n---\nBody\n")

	m := New("sock", root)
	m.Composer.SetValue("/")
	m.refreshCompletions()

	var gitlog, search *CompletionItem
	for i := range m.Candidates {
		switch m.Candidates[i].Label {
		case "/git:log":
			gitlog = &m.Candidates[i]
		case "/search":
			search = &m.Candidates[i]
		}
	}
	if gitlog == nil {
		t.Fatalf("custom command missing from completion: %+v", m.Candidates)
	}
	if gitlog.Detail != "展示提交历史" || gitlog.InsertText != "/git:log " {
		t.Fatalf("custom command metadata wrong: %+v", *gitlog)
	}
	if search == nil {
		t.Fatalf("builtin /search missing: %+v", m.Candidates)
	}
	if search.Detail != "搜索会话内容" {
		t.Fatalf("builtin /search shadowed by custom file: %+v", *search)
	}
	// 无参数提示的内置命令补全不带尾随空格（沿用既有行为）。
	for _, item := range m.Candidates {
		if item.Label == "/sessions" && item.InsertText != "/sessions" {
			t.Fatalf("/sessions insert text changed: %+v", item)
		}
	}
}

func TestCustomCommandHotReloadOnInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	m := New("sock", root)
	m.Composer.SetValue("/ship")
	m.refreshCompletions()
	if len(m.Candidates) != 0 {
		t.Fatalf("command appeared before its file: %+v", m.Candidates)
	}
	writeCommandFile(t, root, "ship.md", "---\ndescription: 发布\n---\n发布 $ARGUMENTS\n")
	m.refreshCompletions()
	if len(m.Candidates) != 1 || m.Candidates[0].Label != "/ship" {
		t.Fatalf("hot reload did not pick up the new command: %+v", m.Candidates)
	}
}

// --- T12.2 /help 列出合并后的全部命令并标注参数提示 ---

func TestHelpListsMergedCommandsWithArgHints(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeCommandFile(t, root, "deploy.md",
		"---\ndescription: 部署服务\nargument-hint: '目标环境'\n---\nDeploy $ARGUMENTS\n")
	m := New("sock", root)
	m.ActiveSession = "s1"
	m.Composer.SetValue("/help")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	if cmd != nil || got.Pending {
		t.Fatalf("/help issued a request: cmd=%v pending=%v", cmd, got.Pending)
	}
	if got.Composer.Value() != "" {
		t.Fatalf("/help left composer text: %q", got.Composer.Value())
	}
	if len(got.Events) != 1 {
		t.Fatalf("help transcript event missing: %+v", got.Events)
	}
	view := projectTranscript(got.Events, 100, false)
	for _, want := range []string{
		"可用命令",
		"/sessions", "/goals", "/search", "/review", "/say", "/reply",
		"/confirm", "/reject", "/plan", "/help",
		"/deploy", "部署服务", "参数：目标环境",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("help text missing %q:\n%s", want, view)
		}
	}
}

// --- T12.3 /plan 发出 plan_mode op（服务端 T10 才实现，先走占位） ---

func TestPlanSendsPlanModeOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	m.Composer.SetValue("/plan")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	if cmd == nil || !got.Pending {
		t.Fatalf("/plan did not submit: cmd=%v pending=%v", cmd, got.Pending)
	}
	if got.Composer.Value() != "" {
		t.Fatalf("/plan left composer text: %q", got.Composer.Value())
	}
	msg, ok := cmd().(resultMsg)
	if !ok || msg.op != "plan_mode" {
		t.Fatalf("plan op mismatch: %T %+v", cmd(), msg)
	}
}

// --- T12.4 自定义提示词命令展开后走普通 chat 路径 ---

func TestCustomPromptCommandExpandsIntoChatPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeCommandFile(t, root, "git/log.md",
		"---\ndescription: 展示提交历史\n---\n请总结 $ARGUMENTS 的提交\n")
	m := New("sock", root)
	m.ActiveSession = "s1"
	m.Composer.SetValue("/git:log 最近改动")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	if cmd == nil || !got.Pending || got.ActiveRunID == "" {
		t.Fatalf("prompt command did not enter the run path: cmd=%v %+v", cmd, got)
	}
	if got.Composer.Value() != "" {
		t.Fatalf("composer not cleared: %q", got.Composer.Value())
	}
	var last sessionlog.Message
	b, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	if json.Unmarshal(b, &last) != nil || last.Role != "user" || last.Text != "请总结 最近改动 的提交" {
		t.Fatalf("expanded prompt missing from transcript: %+v", got.Events)
	}
	entries, err := got.history.List()
	if err != nil || len(entries) != 1 || entries[0].Text != "/git:log 最近改动" {
		t.Fatalf("history should record the typed command: %+v err=%v", entries, err)
	}
	// chat 路径打开运行流，而不是普通请求 op。
	if _, ok := cmd().(runStreamStartedMsg); !ok {
		t.Fatalf("expected run stream command, got %T", cmd())
	}
}

// --- T12.5 未知命令行为不变：仍作为普通消息进入 chat 路径 ---

func TestUnknownCommandStillGoesThroughChatPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := New("sock", t.TempDir())
	m.ActiveSession = "s1"
	m.Composer.SetValue("/xyz 做点事")
	updated, cmd := m.submitComposer()
	got := updated.(Model)
	if cmd == nil || !got.Pending || got.ActiveRunID == "" {
		t.Fatalf("unknown command did not submit as chat: cmd=%v %+v", cmd, got)
	}
	var last sessionlog.Message
	b, _ := json.Marshal(got.Events[len(got.Events)-1].Data)
	if json.Unmarshal(b, &last) != nil || last.Role != "user" || last.Text != "/xyz 做点事" {
		t.Fatalf("unknown command text changed: %+v", got.Events)
	}
	if _, ok := cmd().(runStreamStartedMsg); !ok {
		t.Fatalf("expected run stream command, got %T", cmd())
	}
}

// --- T12.6 /confirm /reject 复用既有提案 op 提交路径 ---

func TestConfirmAndRejectSendProposalOps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ input, op string }{{"/confirm p1", "confirm"}, {"/reject p1", "reject"}} {
		m := New("sock", t.TempDir())
		m.ActiveSession = "s1"
		m.Composer.SetValue(tc.input)
		updated, cmd := m.submitComposer()
		got := updated.(Model)
		if cmd == nil || !got.Pending {
			t.Fatalf("%s did not submit: cmd=%v", tc.input, cmd)
		}
		msg, ok := cmd().(resultMsg)
		if !ok || msg.op != tc.op {
			t.Fatalf("%s op mismatch: %T", tc.input, cmd())
		}
		// 缺提案 ID 时只给用法提示，不发请求。
		m2 := New("sock", t.TempDir())
		m2.Composer.SetValue(strings.TrimSuffix(tc.input, " p1"))
		updated2, cmd2 := m2.submitComposer()
		got2 := updated2.(Model)
		if cmd2 != nil || !strings.Contains(got2.Status, "用法") {
			t.Fatalf("%s without id not guided: cmd=%v status=%q", tc.input, cmd2, got2.Status)
		}
	}
}

// --- T12.7 被拒自定义文件报告只提示一次 ---

func TestCommandReportSurfacesOnceInStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// frontmatter 未闭合的文件会被 loader 拒绝并出报告。
	writeCommandFile(t, root, "broken.md", "---\ndescription: 未闭合\n")
	m := New("sock", root)
	m.Composer.SetValue("/")
	m.refreshCompletions()
	if !strings.Contains(m.Status, "未加载") || !strings.Contains(m.Status, "broken.md") {
		t.Fatalf("rejected report not surfaced: %q", m.Status)
	}
	m.Status = "其他状态"
	m.refreshCompletions()
	if m.Status != "其他状态" {
		t.Fatalf("report surfaced more than once: %q", m.Status)
	}
}
