package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/permission"
	"stable/internal/redact"
	"stable/internal/sessionlog"
	"stable/internal/todo"
)

// newAskFixture builds a session log with one run plus a bare service bound
// to the same project root, the shape the adapters see in production.
func newAskFixture(t *testing.T) (*Service, sessionlog.SessionInfo) {
	t.Helper()
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	return &Service{deps: Deps{ProjectRoot: root}}, session
}

// bindTestClient registers an in-memory subscription the way serveConn does,
// so tests can observe session-scoped pushes without a socket.
func bindTestClient(t *testing.T, svc *Service, sessionID string) chan ServerMsg {
	t.Helper()
	ch := make(chan ServerMsg, 16)
	svc.mu.Lock()
	if svc.clients == nil {
		svc.clients = map[chan ServerMsg]*clientSubscription{}
	}
	svc.clients[ch] = &clientSubscription{ch: ch, sessionID: sessionID}
	svc.mu.Unlock()
	t.Cleanup(func() {
		svc.mu.Lock()
		delete(svc.clients, ch)
		svc.mu.Unlock()
	})
	return ch
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func readPush(t *testing.T, ch chan ServerMsg) ServerMsg {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("no message pushed")
		return ServerMsg{}
	}
}

func assertNoPush(t *testing.T, ch chan ServerMsg) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected push: %+v", msg)
	default:
	}
}

func askQuestions() []execution.QuestionSpec {
	return []execution.QuestionSpec{{
		Question: "用哪个数据库？", Header: "Database",
		Options: []execution.OptionSpec{{Label: "SQLite", Description: "embedded"}, {Label: "Postgres", Description: "server"}},
	}}
}

// Ask blocks until a reply event consumes the question and returns the reply
// as one free-text answer; the waiter count tracks the blocked run.
func TestAskAdapterAskReplyLoop(t *testing.T) {
	svc, session := newAskFixture(t)
	adapter := NewAskAdapter(svc)
	adapter.PollEvery = 2 * time.Millisecond

	type askResult struct {
		resp execution.AskResponse
		err  error
	}
	done := make(chan askResult, 1)
	go func() {
		resp, err := adapter.Ask(context.Background(), execution.AskRequest{SessionID: session.ID, RunID: "run-1", WorkRef: "session-" + session.ID, Questions: askQuestions()})
		done <- askResult{resp, err}
	}()

	waitFor(t, 2*time.Second, func() bool { return svc.askWaiterCount(session.ID) == 1 })
	listed, err := svc.listQuestions(ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Status != sessionlog.QuestionPending {
		t.Fatalf("questions = %+v", listed)
	}
	for _, want := range []string{"[Database]", "用哪个数据库？", "SQLite", "Postgres"} {
		if !strings.Contains(listed[0].Prompt, want) {
			t.Fatalf("prompt %q misses %q", listed[0].Prompt, want)
		}
	}

	if _, err := svc.replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: listed[0].QuestionID, Text: "用 Postgres"}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !result.resp.FreeText || len(result.resp.Answers) != 1 || len(result.resp.Answers[0]) != 1 || result.resp.Answers[0][0] != "用 Postgres" {
			t.Fatalf("ask response = %+v", result.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not return after the reply")
	}
	if got := svc.askWaiterCount(session.ID); got != 0 {
		t.Fatalf("waiter count after reply = %d", got)
	}
	// The reply had a waiter, so no queued user message was appended.
	if hasUserMessage(t, svc, session.ID) {
		t.Fatal("reply with waiter queued an extra message")
	}
}

// Validation failures are errors and a cancelled context leaves the question
// pending in the log for a later queued reply.
func TestAskAdapterValidationAndCancel(t *testing.T) {
	svc, session := newAskFixture(t)
	if _, err := NewAskAdapter(nil).Ask(context.Background(), execution.AskRequest{SessionID: session.ID, Questions: askQuestions()}); err == nil {
		t.Fatal("unbound adapter accepted a question")
	}
	adapter := NewAskAdapter(svc)
	adapter.PollEvery = 2 * time.Millisecond
	if _, err := adapter.Ask(context.Background(), execution.AskRequest{SessionID: session.ID, Questions: nil}); err == nil {
		t.Fatal("empty questions accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Ask(ctx, execution.AskRequest{SessionID: session.ID, RunID: "run-1", WorkRef: "session-" + session.ID, Questions: askQuestions()})
		done <- err
	}()
	waitFor(t, 2*time.Second, func() bool {
		listed, err := svc.listQuestions(ClientMsg{SessionID: session.ID})
		return err == nil && len(listed) == 1
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not react to cancellation")
	}
	listed, err := svc.listQuestions(ClientMsg{SessionID: session.ID})
	if err != nil || len(listed) != 1 || listed[0].Status != sessionlog.QuestionPending {
		t.Fatalf("cancelled question = %+v, err %v", listed, err)
	}
}

// A reply without a waiting run is queued as an ordinary user message; a
// reply to a live ask is not.
func TestReplyQuestionQueuesMessageWithoutWaiter(t *testing.T) {
	svc, session := newAskFixture(t)
	question := sessionlog.PendingQuestion{
		QuestionID: "q-1", WorkRef: "session-" + session.ID, SessionID: session.ID, RunID: "run-1",
		Prompt: "继续吗？", CreatedAt: time.Now().UTC(), Status: sessionlog.QuestionPending,
	}
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, session.ID, sessionlog.EventQuestion, question); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-1", Text: "继续"}); err != nil {
		t.Fatal(err)
	}
	if !hasUserMessage(t, svc, session.ID) {
		t.Fatal("leftover reply was not queued as a user message")
	}

	// A second question answered while a run waits stays reply-only.
	question.QuestionID = "q-2"
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, session.ID, sessionlog.EventQuestion, question); err != nil {
		t.Fatal(err)
	}
	svc.enterAskWait(session.ID)
	if _, err := svc.replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-2", Text: "好了"}); err != nil {
		t.Fatal(err)
	}
	svc.exitAskWait(session.ID)
	replay, err := sessionlog.Replay(svc.deps.ProjectRoot, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventMessage {
			continue
		}
		var msg sessionlog.Message
		if decodeSessionData(event.Data, &msg) == nil && msg.Role == "user" {
			messages++
		}
	}
	if messages != 1 {
		t.Fatalf("queued user messages = %d, want 1", messages)
	}
}

// hasUserMessage reports whether the session log already contains an
// ordinary user text message.
func hasUserMessage(t *testing.T, svc *Service, sessionID string) bool {
	t.Helper()
	replay, err := sessionlog.Replay(svc.deps.ProjectRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventMessage {
			continue
		}
		var msg sessionlog.Message
		if decodeSessionData(event.Data, &msg) == nil && msg.Role == "user" && msg.Text != "" {
			return true
		}
	}
	return false
}

// The poll loop pushes each pending question once per session, retries while
// delivery fails, and stops once the question is answered.
func TestPushPendingQuestionsDedup(t *testing.T) {
	svc, session := newAskFixture(t)
	question := sessionlog.PendingQuestion{
		QuestionID: "q-1", WorkRef: "session-" + session.ID, SessionID: session.ID, RunID: "run-1",
		Prompt: "继续吗？", CreatedAt: time.Now().UTC(), Status: sessionlog.QuestionPending,
	}
	if _, err := sessionlog.Append(svc.deps.ProjectRoot, session.ID, sessionlog.EventQuestion, question); err != nil {
		t.Fatal(err)
	}
	ch := bindTestClient(t, svc, session.ID)

	svc.pushPendingQuestions()
	msg := readPush(t, ch)
	if msg.Type != "questions" || len(msg.Questions) != 1 || msg.Questions[0].QuestionID != "q-1" {
		t.Fatalf("pushed = %+v", msg)
	}
	// While the question stays pending the push is not repeated.
	svc.pushPendingQuestions()
	assertNoPush(t, ch)

	if _, err := svc.replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-1", Text: "继续"}); err != nil {
		t.Fatal(err)
	}
	svc.pushPendingQuestions()
	assertNoPush(t, ch)
}

// The ask adapter's own push marks the question delivered, so the poll loop
// does not double-send it to the same session.
func TestAskAdapterPushDedupsWithPoll(t *testing.T) {
	svc, session := newAskFixture(t)
	adapter := NewAskAdapter(svc)
	adapter.PollEvery = 2 * time.Millisecond
	ch := bindTestClient(t, svc, session.ID)

	done := make(chan error, 1)
	go func() {
		_, err := adapter.Ask(context.Background(), execution.AskRequest{SessionID: session.ID, RunID: "run-1", WorkRef: "session-" + session.ID, Questions: askQuestions()})
		done <- err
	}()
	msg := readPush(t, ch)
	if msg.Type != "questions" || len(msg.Questions) != 1 {
		t.Fatalf("pushed = %+v", msg)
	}
	svc.pushPendingQuestions()
	assertNoPush(t, ch)

	listed, err := svc.listQuestions(ClientMsg{SessionID: session.ID})
	if err != nil || len(listed) != 1 {
		t.Fatalf("questions = %+v, err %v", listed, err)
	}
	if _, err := svc.replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: listed[0].QuestionID, Text: "好的"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not return after the reply")
	}
}

// Pending plan approvals ride the same poll with their own dedup state and
// stop once the approval resolves. The immediate plan_approval_pending
// broadcast from the submission stays visible; the poll never repeats it.
func TestPushPendingPlanApprovalsDedup(t *testing.T) {
	svc, session := newAskFixture(t)
	ch := bindTestClient(t, svc, session.ID)

	if _, err := svc.SubmitPlanApproval(session.ID, "run-1", filepath.Join(svc.deps.ProjectRoot, ".stable/plans", session.ID+".md")); err != nil {
		t.Fatal(err)
	}
	if immediate := readPush(t, ch); immediate.Type != "plan_approval_pending" {
		t.Fatalf("submission push = %+v", immediate)
	}
	svc.pushPendingPlanApprovals()
	msg := readPush(t, ch)
	if msg.Type != "plan_approvals" || len(msg.PlanApprovals) != 1 || msg.PlanApprovals[0].RunID != "run-1" {
		t.Fatalf("pushed = %+v", msg)
	}
	svc.pushPendingPlanApprovals()
	assertNoPush(t, ch)

	if _, _, _, err := svc.ResolvePlanApproval(session.ID, PlanResolveAuto, ""); err != nil {
		t.Fatal(err)
	}
	if resolved := readPush(t, ch); resolved.Type != "plan_approval_resolved" {
		t.Fatalf("resolution push = %+v", resolved)
	}
	svc.pushPendingPlanApprovals()
	assertNoPush(t, ch)
}

// The todo provider journals every change as a full snapshot event with a
// strictly increasing revision, pushes it to the session clients, redacts
// credentials from the persisted file, and continues the revision sequence
// after a provider restart.
func TestTodoProviderJournalAndBroadcast(t *testing.T) {
	svc, session := newAskFixture(t)
	svc.deps.ProviderCredential = "sk-test-secret-1234567890"
	ch := bindTestClient(t, svc, session.ID)

	provider := NewTodoProvider(svc)
	list := provider.For(session.ID)
	if list == nil {
		t.Fatal("todo provider returned no task list")
	}
	if NewTodoProvider(nil).For(session.ID) != nil {
		t.Fatal("unbound provider served a task list")
	}
	task, err := list.Create("写计划 sk-test-secret-1234567890", "描述", "编写中", nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := readPush(t, ch)
	if msg.Type != "todo" || len(msg.Tasks) != 1 || msg.Tasks[0].Subject != "写计划 "+redact.Placeholder {
		t.Fatalf("todo push = %+v", msg)
	}
	if _, err := list.Update(task.ID, todo.UpdatePatch{Status: ptrTodoStatus(todo.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	assertTodoRevisions(t, svc, session.ID, 1, 2)

	data, err := os.ReadFile(filepath.Join(svc.deps.ProjectRoot, todo.DirName, session.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sk-test-secret-1234567890") {
		t.Fatalf("credential leaked into the task file: %s", data)
	}

	// A fresh provider instance over the same log continues the revisions.
	restarted := NewTodoProvider(svc)
	relist := restarted.For(session.ID)
	if _, err := relist.Create("收尾", "", "", nil); err != nil {
		t.Fatal(err)
	}
	assertTodoRevisions(t, svc, session.ID, 1, 2, 3)
}

func assertTodoRevisions(t *testing.T, svc *Service, sessionID string, want ...int) {
	t.Helper()
	replay, err := sessionlog.Replay(svc.deps.ProjectRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventTodo {
			continue
		}
		var update sessionlog.TodoUpdate
		if decodeSessionData(event.Data, &update) == nil {
			got = append(got, update.Revision)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("todo revisions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("todo revisions = %v, want %v", got, want)
		}
	}
}

func ptrTodoStatus(status todo.Status) *todo.Status { return &status }

// The executor factory accepts the exact wiring the two production call
// sites use — sinks in the deps literal plus WithPlanSink — and the bound
// adapters serve the session afterwards.
func TestExecutorFactoryWiring(t *testing.T) {
	svc, session := newAskFixture(t)
	askSink := NewAskAdapter(nil)
	todoProvider := NewTodoProvider(nil)
	planSink := NewPlanApprovalSink(nil)

	formal := t.TempDir()
	authority := permission.Authority{
		RunID: "run-1", SessionID: session.ID,
		AllowedRoot: formal, FormalRoot: formal,
		CandidateRoot: filepath.Join(t.TempDir(), "cand"),
		Mode:          permission.ModeDefault,
	}
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Now:          time.Now,
		QuestionSink: askSink,
		TodoProvider: todoProvider,
	}, execution.WithPlanSink(planSink))
	runner, err := factory.ForRun(agent.ExecutionRequest{
		RunID:            authority.RunID,
		Work:             agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID},
		Intent:           "test",
		Model:            "test",
		PermissionBounds: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		t.Fatal("factory returned no executor")
	}

	askSink.Bind(svc)
	todoProvider.Bind(svc)
	planSink.Bind(svc)
	if list := todoProvider.For(session.ID); list == nil {
		t.Fatal("bound todo provider returned no task list")
	}
	if _, err := NewPlanApprovalSink(nil).SubmitPlan(context.Background(), session.ID, "run-1", "plan.md"); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound plan sink = %v", err)
	}
	// The bound sink submits the plan and the blocking run continues once
	// the approval resolves.
	type planResult struct {
		choice string
		err    error
	}
	done := make(chan planResult, 1)
	go func() {
		choice, err := planSink.SubmitPlan(context.Background(), session.ID, "run-1", filepath.Join(svc.deps.ProjectRoot, ".stable/plans", session.ID+".md"))
		done <- planResult{choice, err}
	}()
	waitFor(t, 2*time.Second, func() bool {
		svc.planMu.Lock()
		defer svc.planMu.Unlock()
		return svc.pendingPlanApprovalLocked(session.ID) != nil
	})
	if _, _, _, err := svc.ResolvePlanApproval(session.ID, PlanResolveAuto, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.choice != execution.PlanChoiceAuto {
			t.Fatalf("plan result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan sink did not return after the approval")
	}
}
