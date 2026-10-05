package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/planfile"
	"stable/internal/redact"
	"stable/internal/todo"
)

// policyGate exercises the real permission policy without approval storage,
// so plan-file authorization follows the exact rules the production gate
// applies.
type policyGate struct{ policy permission.Policy }

func (g policyGate) Authorize(_ context.Context, a permission.Authority, op permission.Operation) (permission.PermissionDecision, error) {
	return g.policy.Decide(a, op), nil
}

type fakeQuestionSink struct {
	req   AskRequest
	resp  AskResponse
	err   error
	calls int
}

func (s *fakeQuestionSink) Ask(_ context.Context, req AskRequest) (AskResponse, error) {
	s.calls++
	s.req = req
	return s.resp, s.err
}

type fakePlanSink struct {
	sessionID string
	runID     string
	planPath  string
	choice    string
	err       error
	calls     int
}

func (s *fakePlanSink) SubmitPlan(_ context.Context, sessionID, runID, planPath string) (string, error) {
	s.calls++
	s.sessionID, s.runID, s.planPath = sessionID, runID, planPath
	return s.choice, s.err
}

// fakeTodoProvider serves real task lists backed by a temporary directory,
// standing in for the conversation-side TodoProvider.
type fakeTodoProvider struct {
	root  string
	lists map[string]*todo.TaskList
}

func newFakeTodoProvider(t *testing.T) *fakeTodoProvider {
	t.Helper()
	return &fakeTodoProvider{root: t.TempDir(), lists: map[string]*todo.TaskList{}}
}

func (p *fakeTodoProvider) For(sessionID string) *todo.TaskList {
	if list, ok := p.lists[sessionID]; ok {
		return list
	}
	list := todo.NewTaskList(p.root, sessionID, nil)
	p.lists[sessionID] = list
	return list
}

type nilTodoProvider struct{}

func (nilTodoProvider) For(string) *todo.TaskList { return nil }

func m06Authority(t *testing.T, formal string, mode permission.Mode, planPath string) permission.Authority {
	t.Helper()
	return permission.Authority{
		RunID:         "run-1",
		SessionID:     "0123456789abcdef0123456789abcdef",
		AllowedRoot:   formal,
		FormalRoot:    formal,
		CandidateRoot: filepath.Join(t.TempDir(), "cand"),
		Mode:          mode,
		PlanFilePath:  planPath,
	}
}

func m06Request(t *testing.T, authority permission.Authority) agent.ExecutionRequest {
	t.Helper()
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	return agent.ExecutionRequest{
		RunID:            authority.RunID,
		Work:             agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID},
		Intent:           "test",
		Model:            "test",
		PermissionBounds: raw,
	}
}

func m06Runner(t *testing.T, authority permission.Authority, deps ToolExecutorDeps) agent.RunExecutor {
	t.Helper()
	deps.Now = time.Now
	factory := NewToolExecutorFactory(deps)
	runner, err := factory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func m06Call(name, arguments string) llm.ToolUse {
	return llm.ToolUse{ID: "call-" + name, Name: name, Arguments: json.RawMessage(arguments)}
}

func requireOutcome(t *testing.T, outcome agent.ToolOutcome, err error, wantError bool, wantParts ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if outcome.IsError != wantError {
		t.Fatalf("outcome IsError = %v, want %v (content %q)", outcome.IsError, wantError, outcome.Content)
	}
	if wantError && outcome.Status != agent.ToolFailed && outcome.Status != agent.ToolDenied {
		t.Fatalf("error outcome status = %q", outcome.Status)
	}
	if !wantError && outcome.Status != agent.ToolSucceeded {
		t.Fatalf("success outcome status = %q, content %q", outcome.Status, outcome.Content)
	}
	for _, part := range wantParts {
		if !strings.Contains(outcome.Content, part) {
			t.Fatalf("outcome content %q missing %q", outcome.Content, part)
		}
	}
}

func TestPlanFileWriteBypassesCandidate(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModePlan, "")
	planPath, _, err := planfile.Ensure(formal, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.PlanFilePath = planPath
	credential := "provider-secret-key-01"
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{}, ProviderCredential: credential})

	arguments, err := json.Marshal(map[string]any{"file_path": planPath, "content": "# Plan\nkey " + credential + " end\n"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("write_file", string(arguments)))
	requireOutcome(t, outcome, err, false, "计划文件已更新")

	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), redact.Placeholder) || strings.Contains(string(data), credential) {
		t.Fatalf("plan file not redacted: %q", string(data))
	}
	// The plan write must never create a candidate or a snapshot.
	if _, statErr := os.Stat(authority.CandidateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("candidate root was created for a plan write: %v", statErr)
	}
	if outcome.Diff != nil || len(outcome.Snapshots) != 0 {
		t.Fatalf("plan write produced diff or snapshots: %#v", outcome)
	}
}

func TestPlanFileWriteDeniedOutsidePlanMode(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	planPath, _, err := planfile.Ensure(formal, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.PlanFilePath = planPath
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{}})

	arguments, err := json.Marshal(map[string]any{"file_path": planPath, "content": "# Plan\n"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("write_file", string(arguments)))
	requireOutcome(t, outcome, err, true, "writes are restricted to the isolated candidate")
	if outcome.Status != agent.ToolDenied {
		t.Fatalf("outcome status = %q, want denied", outcome.Status)
	}
	data, err := os.ReadFile(planPath)
	if err != nil || len(data) != 0 {
		t.Fatalf("plan file changed by denied write: %q, err %v", string(data), err)
	}
	if _, statErr := os.Stat(authority.CandidateRoot); !os.IsNotExist(statErr) {
		t.Fatal("candidate root was created for a denied plan write")
	}
}

func TestPlanFileEditReplacesOnceAndStaysPrivate(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModePlan, "")
	planPath, _, err := planfile.Ensure(formal, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.PlanFilePath = planPath
	initial := "# Plan\n\n- step one\n- step two\n"
	if err := os.WriteFile(planPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{}})

	arguments, err := json.Marshal(map[string]any{"file_path": planPath, "old_string": "- step one", "new_string": "- step one done"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("edit_file", string(arguments)))
	requireOutcome(t, outcome, err, false, "计划文件已更新")

	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Plan\n\n- step one done\n- step two\n"; string(data) != want {
		t.Fatalf("plan file = %q, want %q", string(data), want)
	}
	info, err := os.Lstat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	// Ambiguous and missing anchors are refused without touching the file.
	ambiguous, err := json.Marshal(map[string]any{"file_path": planPath, "old_string": "step", "new_string": "x"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = runner.Execute(context.Background(), m06Call("edit_file", string(ambiguous)))
	requireOutcome(t, outcome, err, true, "must be unique")
	missing, err := json.Marshal(map[string]any{"file_path": planPath, "old_string": "absent", "new_string": "x"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = runner.Execute(context.Background(), m06Call("edit_file", string(missing)))
	requireOutcome(t, outcome, err, true, "old_string not found")
}

func TestPlanFileWriteRefusesShortCredential(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModePlan, "")
	planPath, _, err := planfile.Ensure(formal, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.PlanFilePath = planPath
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{}, ProviderCredential: "short"})

	arguments, err := json.Marshal(map[string]any{"file_path": planPath, "content": "# Plan\n"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("write_file", string(arguments)))
	requireOutcome(t, outcome, err, true, "cannot be redacted safely")
	data, statErr := os.ReadFile(planPath)
	if statErr != nil || len(data) != 0 {
		t.Fatalf("plan file was written despite unsafe credential: %q, err %v", string(data), statErr)
	}
}

func TestAskUserBranch(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	questions := `{"questions":[{"question":"Which database?","header":"Database","options":[{"label":"SQLite","description":"embedded"},{"label":"Postgres","description":"server"}],"multiSelect":false}]}`

	sink := &fakeQuestionSink{resp: AskResponse{Answers: [][]string{{"Postgres"}}}}
	runner := m06Runner(t, authority, ToolExecutorDeps{QuestionSink: sink})
	outcome, err := runner.Execute(context.Background(), m06Call("ask_user", questions))
	requireOutcome(t, outcome, err, false, "Q: Which database?", "A: Postgres")
	if sink.calls != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.calls)
	}
	if sink.req.SessionID != authority.SessionID || sink.req.RunID != authority.RunID {
		t.Fatalf("ask request = %#v", sink.req)
	}
	if want := "session-" + authority.SessionID; sink.req.WorkRef != want {
		t.Fatalf("workRef = %q, want %q", sink.req.WorkRef, want)
	}
	if len(sink.req.Questions) != 1 || len(sink.req.Questions[0].Options) != 2 {
		t.Fatalf("ask questions = %#v", sink.req.Questions)
	}

	// Multi-select answers are joined.
	multi := &fakeQuestionSink{resp: AskResponse{Answers: [][]string{{"SQLite", "Postgres"}}}}
	runner = m06Runner(t, authority, ToolExecutorDeps{QuestionSink: multi})
	outcome, err = runner.Execute(context.Background(), m06Call("ask_user", questions))
	requireOutcome(t, outcome, err, false, "A: SQLite, Postgres")

	// Cancellation is a plain result, not an error.
	cancelled := &fakeQuestionSink{err: context.Canceled}
	runner = m06Runner(t, authority, ToolExecutorDeps{QuestionSink: cancelled})
	outcome, err = runner.Execute(context.Background(), m06Call("ask_user", questions))
	requireOutcome(t, outcome, err, false, "Question cancelled")

	// Nil sink makes the tool unavailable.
	runner = m06Runner(t, authority, ToolExecutorDeps{})
	outcome, err = runner.Execute(context.Background(), m06Call("ask_user", questions))
	requireOutcome(t, outcome, err, true, "提问通道不可用")
}

func TestAskUserWorkRefJoinsGoalIdentifiers(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	authority.GoalID = "goal-1"
	authority.WorkItemID = "item-1"
	request := m06Request(t, authority)
	request.Work = agent.WorkRef{Kind: agent.WorkGoal, SessionID: authority.SessionID, GoalID: "goal-1", WorkItemID: "item-1"}
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = raw
	sink := &fakeQuestionSink{resp: AskResponse{Answers: [][]string{{"yes"}}}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now, QuestionSink: sink})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), m06Call("ask_user", `{"questions":[{"question":"Go on?","header":"Confirm","options":[{"label":"Yes","description":"continue"},{"label":"No","description":"stop"}],"multiSelect":false}]}`))
	requireOutcome(t, outcome, err, false, "A: yes")
	if sink.req.WorkRef != "goal-1/item-1" {
		t.Fatalf("workRef = %q, want goal-1/item-1", sink.req.WorkRef)
	}
}

func TestAskUserValidation(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	sink := &fakeQuestionSink{}
	runner := m06Runner(t, authority, ToolExecutorDeps{QuestionSink: sink})
	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"too many questions", `{"questions":[` + strings.Repeat(`{"question":"q","header":"h","options":[{"label":"a"},{"label":"b"}]},`, 4) + `{"question":"q","header":"h","options":[{"label":"a"},{"label":"b"}]}]}`, "at most 4"},
		{"missing header", `{"questions":[{"question":"q","options":[{"label":"a"},{"label":"b"}]}]}`, "missing header"},
		{"missing options", `{"questions":[{"question":"q","header":"h"}]}`, "missing options"},
		{"option missing label", `{"questions":[{"question":"q","header":"h","options":[{"description":"d"},{"label":"b"}]}]}`, "missing label"},
		{"too few options", `{"questions":[{"question":"q","header":"h","options":[{"label":"only"}]}]}`, "between 2 and 4"},
		{"missing questions", `{}`, "questions is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := runner.Execute(context.Background(), m06Call("ask_user", tc.arguments))
			requireOutcome(t, outcome, err, true, tc.want)
		})
	}
	if sink.calls != 0 {
		t.Fatalf("sink called %d times for invalid questions", sink.calls)
	}
}

func TestExitPlanModeBranch(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModePlan, "")
	planPath, _, err := planfile.Ensure(formal, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.PlanFilePath = planPath

	sink := &fakePlanSink{choice: PlanChoiceManual}
	runner := m06Runner(t, authority, ToolExecutorDeps{PlanSink: sink})
	outcome, err := runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, false, "计划已批准", "请结束本回合")
	if sink.calls != 1 || sink.sessionID != authority.SessionID || sink.runID != authority.RunID || sink.planPath != planPath {
		t.Fatalf("submit plan request = %#v", sink)
	}

	feedback := &fakePlanSink{err: PlanFeedbackError{Text: "add a test step"}}
	runner = m06Runner(t, authority, ToolExecutorDeps{PlanSink: feedback})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, false, "用户要求继续修改计划:add a test step,请保持计划模式")

	cancelled := &fakePlanSink{err: PlanCancelledError{}}
	runner = m06Runner(t, authority, ToolExecutorDeps{PlanSink: cancelled})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, false, "计划审批已取消")

	runCancelled := &fakePlanSink{err: context.Canceled}
	runner = m06Runner(t, authority, ToolExecutorDeps{PlanSink: runCancelled})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, false, "Question cancelled")

	// Nil sink behind an existing plan file is a configuration error.
	runner = m06Runner(t, authority, ToolExecutorDeps{})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, true, "审批通道不可用")

	// A missing plan file is reported before the sink is consulted.
	noPlan := &fakePlanSink{choice: PlanChoiceAuto}
	missing := authority
	missing.PlanFilePath = filepath.Join(formal, planfile.DirName, "absent.md")
	runner = m06Runner(t, missing, ToolExecutorDeps{PlanSink: noPlan})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, true, "计划文件尚未创建")
	if noPlan.calls != 0 {
		t.Fatal("plan sink called without a plan file")
	}

	// Outside plan mode the tool is refused outright.
	notPlan := authority
	notPlan.PlanFilePath = ""
	runner = m06Runner(t, notPlan, ToolExecutorDeps{PlanSink: &fakePlanSink{choice: PlanChoiceAuto}})
	outcome, err = runner.Execute(context.Background(), m06Call("exit_plan_mode", `{}`))
	requireOutcome(t, outcome, err, true, "当前不在计划模式")
}

func TestTaskToolBranches(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	provider := newFakeTodoProvider(t)
	runner := m06Runner(t, authority, ToolExecutorDeps{TodoProvider: provider})

	outcome, err := runner.Execute(context.Background(), m06Call("task_create", `{"subject":"Write parser","description":"Parse the config file","activeForm":"Writing parser"}`))
	requireOutcome(t, outcome, err, false, "Created task", "[pending]", "Write parser")
	firstID := taskIDFromBody(t, outcome.Content)

	outcome, err = runner.Execute(context.Background(), m06Call("task_create", `{"subject":"Write tests","description":"Cover the parser"}`))
	requireOutcome(t, outcome, err, false, "Write tests")
	secondID := taskIDFromBody(t, outcome.Content)

	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"taskId":"`+firstID+`","status":"in_progress"}`))
	requireOutcome(t, outcome, err, false, "[in_progress]", "Write parser")

	outcome, err = runner.Execute(context.Background(), m06Call("task_list", `{}`))
	requireOutcome(t, outcome, err, false, "* "+firstID, "  "+secondID)

	outcome, err = runner.Execute(context.Background(), m06Call("task_get", `{"taskId":"`+firstID+`"}`))
	requireOutcome(t, outcome, err, false, firstID, "description: Parse the config file", "active form: Writing parser")

	// Dependencies and metadata flow through to the task list.
	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"taskId":"`+firstID+`","addBlocks":["`+secondID+`"],"metadata":{"priority":"high"}}`))
	requireOutcome(t, outcome, err, false, "blocks: "+secondID, "metadata: priority=high")
	outcome, err = runner.Execute(context.Background(), m06Call("task_get", `{"taskId":"`+firstID+`"}`))
	requireOutcome(t, outcome, err, false, "metadata: priority=high")

	// Metadata replacement and deletion.
	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"taskId":"`+firstID+`","metadata":{}}`))
	requireOutcome(t, outcome, err, false, "Updated task")
	if strings.Contains(outcome.Content, "priority") {
		t.Fatalf("metadata not cleared: %q", outcome.Content)
	}
	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"taskId":"`+secondID+`","status":"deleted"}`))
	requireOutcome(t, outcome, err, false, "Deleted task "+secondID)
	outcome, err = runner.Execute(context.Background(), m06Call("task_get", `{"taskId":"`+secondID+`"}`))
	requireOutcome(t, outcome, err, true, "task not found")

	// Argument errors.
	outcome, err = runner.Execute(context.Background(), m06Call("task_create", `{"description":"no subject"}`))
	requireOutcome(t, outcome, err, true, "subject is required")
	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"status":"completed"}`))
	requireOutcome(t, outcome, err, true, "taskId is required")
	outcome, err = runner.Execute(context.Background(), m06Call("task_update", `{"taskId":"`+firstID+`","metadata":{"k":1}}`))
	requireOutcome(t, outcome, err, true, "string values")
}

func TestTaskToolsWithoutProvider(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{})
	outcome, err := runner.Execute(context.Background(), m06Call("task_list", `{}`))
	requireOutcome(t, outcome, err, true, "任务清单通道不可用")

	nilProvider := m06Runner(t, authority, ToolExecutorDeps{TodoProvider: nilTodoProvider{}})
	outcome, err = nilProvider.Execute(context.Background(), m06Call("task_create", `{"subject":"s","description":"d"}`))
	requireOutcome(t, outcome, err, true, "任务清单通道不可用")
}

// taskIDFromBody extracts the task id from a "Created task:" rendering.
func taskIDFromBody(t *testing.T, content string) string {
	t.Helper()
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected create result %q", content)
	}
	id := strings.Fields(lines[1])
	if len(id) == 0 || !strings.HasPrefix(id[0], "task-") {
		t.Fatalf("no task id in %q", content)
	}
	return id[0]
}
