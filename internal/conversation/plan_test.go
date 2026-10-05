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
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/planfile"
	"stable/internal/prompt"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// newPlanFixture builds a project with one session already in plan mode
// (plan file ensured, one user_toggle event recorded).
func newPlanFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{Store: db, ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{}}
	if _, err := svc.SetPlanMode(session.ID, sessionlog.PlanModePlan, sessionlog.PlanModeReasonUserToggle); err != nil {
		t.Fatal(err)
	}
	return svc, root, session.ID
}

func newTerminalRunner(sessionID, runID string) *fixedRunner {
	events := make(chan agent.ExecutionEvent, 1)
	done := make(chan agent.RunOutcome, 1)
	events <- agent.ExecutionEvent{ID: "evt-end", RunID: runID, SessionID: sessionID, RunSeq: 1, At: time.Now().UTC(), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`)}
	close(events)
	done <- agent.RunOutcome{RunID: runID, Status: agent.RunCompleted}
	close(done)
	return &fixedRunner{handle: &agent.RunHandle{Events: events, Done: done}}
}

func waitRunOutcome(t *testing.T, updates chan ServerMsg) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-updates:
			if msg.Type == "run_outcome" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for run outcome")
		}
	}
}

func decodePlanEvents(t *testing.T, events []sessionlog.Event) (modes []sessionlog.PlanMode, records []sessionlog.PlanApprovalRecord, messages []sessionlog.Message) {
	t.Helper()
	for _, e := range events {
		raw, _ := json.Marshal(e.Data)
		switch e.Type {
		case sessionlog.EventPlanMode:
			var m sessionlog.PlanMode
			if json.Unmarshal(raw, &m) == nil {
				modes = append(modes, m)
			}
		case sessionlog.EventPlanApproval:
			var r sessionlog.PlanApprovalRecord
			if json.Unmarshal(raw, &r) == nil {
				records = append(records, r)
			}
		case sessionlog.EventMessage:
			var m sessionlog.Message
			if json.Unmarshal(raw, &m) == nil {
				messages = append(messages, m)
			}
		}
	}
	return modes, records, messages
}

// The plan_mode op toggles the runtime state, creates the plan file on the
// way in, keeps the plan path on the way out, and records every transition.
func TestPlanModeToggleOp(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{ProjectRoot: root}}
	if state := svc.PlanStateOf(session.ID); state.Mode != sessionlog.PlanModeDefault || state.PlanPath != "" || state.ExecutionMode != "" || state.Runs != 0 {
		t.Fatalf("initial plan state = %+v", state)
	}
	msgs, err := svc.handle(context.Background(), ClientMsg{Op: "plan_mode", SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Type != "plan_state" || msgs[0].PlanState == nil {
		t.Fatalf("plan_mode response = %+v", msgs)
	}
	planPath, _, err := planfile.Ensure(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state := *msgs[0].PlanState; state.Mode != sessionlog.PlanModePlan || state.PlanPath != planPath {
		t.Fatalf("plan state after toggle = %+v", state)
	}
	if exists, err := planfile.Exists(root, session.ID); err != nil || !exists {
		t.Fatalf("plan file not ensured: %t %v", exists, err)
	}
	msgs, err = svc.handle(context.Background(), ClientMsg{Op: "plan_mode", SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if state := *msgs[0].PlanState; state.Mode != sessionlog.PlanModeDefault || state.PlanPath != planPath {
		t.Fatalf("plan state after second toggle = %+v", state)
	}
	if state := svc.PlanStateOf(session.ID); state.Mode != sessionlog.PlanModeDefault || state.PlanPath != planPath {
		t.Fatalf("service plan state = %+v", state)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	modes, _, _ := decodePlanEvents(t, transcript.Events)
	if len(modes) != 2 ||
		modes[0].Mode != sessionlog.PlanModePlan || modes[0].Reason != sessionlog.PlanModeReasonUserToggle ||
		modes[1].Mode != sessionlog.PlanModeDefault || modes[1].Reason != sessionlog.PlanModeReasonUserToggle {
		t.Fatalf("plan mode events = %+v", modes)
	}
}

func TestSessionCreateInitializesDefaultPlanState(t *testing.T) {
	root := t.TempDir()
	svc := &Service{deps: Deps{ProjectRoot: root}}
	msgs, err := svc.handle(context.Background(), ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Session == nil {
		t.Fatalf("session_create response = %+v", msgs)
	}
	if state := svc.PlanStateOf(msgs[0].Session.ID); state.Mode != sessionlog.PlanModeDefault || state.PlanPath != "" || state.Runs != 0 {
		t.Fatalf("created session plan state = %+v", state)
	}
}

func TestPlanOpsValidateClient(t *testing.T) {
	if err := validateClient(ClientMsg{Op: "plan_mode"}); err == nil {
		t.Fatal("plan_mode without session accepted")
	}
	if err := validateClient(ClientMsg{Op: "plan_resolve", SessionID: "s"}); err == nil {
		t.Fatal("plan_resolve without choice accepted")
	}
	if err := validateClient(ClientMsg{Op: "plan_resolve", SessionID: "s", ApprovalChoice: "nope"}); err == nil {
		t.Fatal("invalid choice accepted")
	}
	if err := validateClient(ClientMsg{Op: "plan_resolve", SessionID: "s", ApprovalChoice: PlanResolveFeedback}); err == nil {
		t.Fatal("feedback choice without text accepted")
	}
	for _, choice := range []string{PlanResolveAuto, PlanResolveManual, PlanResolveCancel} {
		if err := validateClient(ClientMsg{Op: "plan_resolve", SessionID: "s", ApprovalChoice: choice}); err != nil {
			t.Fatalf("choice %s rejected: %v", choice, err)
		}
	}
	if err := validateClient(ClientMsg{Op: "plan_resolve", SessionID: "s", ApprovalChoice: PlanResolveFeedback, Text: "改一下"}); err != nil {
		t.Fatalf("feedback with text rejected: %v", err)
	}
}

// Every plan_resolve choice reaches its terminal event, inserts the ordinary
// user message into the session, and moves the plan state accordingly.
func TestPlanResolveBranches(t *testing.T) {
	cases := []struct {
		choice     string
		feedback   string
		wantText   string
		wantMode   string
		wantExec   string
		wantStatus string
	}{
		{PlanResolveAuto, "", "计划已批准，后续按自动接受模式执行", sessionlog.PlanModeDefault, PlanExecutionAcceptEdits, sessionlog.PlanApprovalApprovedAuto},
		{PlanResolveManual, "", "计划已批准，后续按逐次确认模式执行", sessionlog.PlanModeDefault, PlanExecutionDefault, sessionlog.PlanApprovalApprovedManual},
		{PlanResolveFeedback, "先补充验证章节", "用户要求继续修改计划：先补充验证章节", sessionlog.PlanModePlan, "", sessionlog.PlanApprovalFeedback},
		{PlanResolveCancel, "", "计划审批已取消", sessionlog.PlanModePlan, "", sessionlog.PlanApprovalCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.choice, func(t *testing.T) {
			svc, root, sessionID := newPlanFixture(t)
			planPath := svc.PlanStateOf(sessionID).PlanPath
			approval, err := svc.SubmitPlanApproval(sessionID, "run-1", planPath)
			if err != nil {
				t.Fatal(err)
			}
			if approval.Status != sessionlog.PlanApprovalSubmitted || approval.SessionID != sessionID || approval.RunID != "run-1" || approval.PlanPath != planPath {
				t.Fatalf("submitted approval = %+v", approval)
			}
			// The replace policy refuses a second submission while one is pending.
			if _, err := svc.SubmitPlanApproval(sessionID, "run-2", planPath); err == nil || !strings.Contains(err.Error(), "已有待审批计划") {
				t.Fatalf("second submission = %v", err)
			}
			resolved, state, text, err := svc.ResolvePlanApproval(sessionID, tc.choice, tc.feedback)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.ID != approval.ID || resolved.Status != tc.wantStatus || resolved.ResolvedAt.IsZero() {
				t.Fatalf("resolved approval = %+v", resolved)
			}
			if tc.choice == PlanResolveFeedback && resolved.Feedback != tc.feedback {
				t.Fatalf("feedback not recorded: %+v", resolved)
			}
			if text != tc.wantText {
				t.Fatalf("inserted text = %q, want %q", text, tc.wantText)
			}
			if state.Mode != tc.wantMode || state.ExecutionMode != tc.wantExec {
				t.Fatalf("resolved state = %+v", state)
			}
			if state := svc.PlanStateOf(sessionID); state.Mode != tc.wantMode || state.ExecutionMode != tc.wantExec {
				t.Fatalf("service state = %+v", state)
			}
			transcript, err := sessionlog.Replay(root, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			modes, records, messages := decodePlanEvents(t, transcript.Events)
			if len(records) != 2 ||
				records[0].RequestID != approval.ID || records[0].Status != sessionlog.PlanApprovalSubmitted || !records[0].ResolvedAt.IsZero() ||
				records[1].RequestID != approval.ID || records[1].Status != tc.wantStatus || records[1].ResolvedAt.IsZero() {
				t.Fatalf("plan approval events = %+v", records)
			}
			wantModes := 1
			if tc.wantStatus == sessionlog.PlanApprovalApprovedAuto || tc.wantStatus == sessionlog.PlanApprovalApprovedManual {
				wantModes = 2
			}
			if len(modes) != wantModes {
				t.Fatalf("plan mode events = %+v", modes)
			}
			if wantModes == 2 && (modes[1].Mode != sessionlog.PlanModeDefault || modes[1].Reason != sessionlog.PlanModeReasonPlanApproved) {
				t.Fatalf("approval plan mode event = %+v", modes[1])
			}
			if len(messages) == 0 || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Text != tc.wantText {
				t.Fatalf("session messages = %+v", messages)
			}
			// A resolved request is gone from the pending set.
			if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveCancel, ""); err == nil || !strings.Contains(err.Error(), "没有待审批计划") {
				t.Fatalf("double resolve = %v", err)
			}
			if status, _, ok := svc.PlanApprovalStatus(approval.ID); !ok || status != tc.wantStatus {
				t.Fatalf("tracked status = %q ok=%t", status, ok)
			}
		})
	}
}

func TestPlanResolveOwnershipAndPendingChecks(t *testing.T) {
	svc, root, sessionID := newPlanFixture(t)
	other, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	planPath := svc.PlanStateOf(sessionID).PlanPath
	if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveAuto, ""); err == nil || !strings.Contains(err.Error(), "没有待审批计划") {
		t.Fatalf("resolve without pending = %v", err)
	}
	approval, err := svc.SubmitPlanApproval(sessionID, "run-1", planPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.ResolvePlanApproval(other.ID, PlanResolveAuto, ""); err == nil || !strings.Contains(err.Error(), "没有待审批计划") {
		t.Fatalf("cross-session resolve = %v", err)
	}
	if _, _, ok := svc.PlanApprovalStatus(approval.ID); !ok {
		t.Fatal("submitted request not tracked")
	}
	if _, _, ok := svc.PlanApprovalStatus("missing"); ok {
		t.Fatal("unknown request tracked")
	}
	// An invalid choice is refused without touching the pending request.
	if _, _, _, err := svc.ResolvePlanApproval(sessionID, "maybe", ""); err == nil {
		t.Fatal("invalid choice accepted")
	}
	if status, _, _ := svc.PlanApprovalStatus(approval.ID); status != sessionlog.PlanApprovalSubmitted {
		t.Fatalf("pending status changed to %q", status)
	}
}

func TestPlanResolveOpReturnsStateAndMessage(t *testing.T) {
	svc, _, sessionID := newPlanFixture(t)
	planPath := svc.PlanStateOf(sessionID).PlanPath
	if _, err := svc.SubmitPlanApproval(sessionID, "run-1", planPath); err != nil {
		t.Fatal(err)
	}
	msgs, err := svc.handle(context.Background(), ClientMsg{Op: "plan_resolve", SessionID: sessionID, ApprovalChoice: PlanResolveManual})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Type != "plan_state" || msgs[0].PlanState == nil || msgs[1].Type != "message" || msgs[1].Message == nil {
		t.Fatalf("plan_resolve response = %+v", msgs)
	}
	if msgs[0].PlanState.Mode != sessionlog.PlanModeDefault || msgs[0].PlanState.ExecutionMode != PlanExecutionDefault {
		t.Fatalf("plan state = %+v", msgs[0].PlanState)
	}
	if msgs[1].Message.Role != core.MessageRoleUser || msgs[1].Message.Text != "计划已批准，后续按逐次确认模式执行" {
		t.Fatalf("inserted message = %+v", msgs[1].Message)
	}
}

func TestSessionLoadCarriesPlanState(t *testing.T) {
	svc, root, sessionID := newPlanFixture(t)
	other, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := svc.handle(context.Background(), ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Plan == nil {
		t.Fatalf("session_load response = %+v", msgs)
	}
	if msgs[0].Plan.Mode != sessionlog.PlanModePlan || msgs[0].Plan.PlanPath != svc.PlanStateOf(sessionID).PlanPath {
		t.Fatalf("loaded plan state = %+v", msgs[0].Plan)
	}
	msgs, err = svc.handle(context.Background(), ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0].Plan == nil || msgs[0].Plan.Mode != sessionlog.PlanModeDefault {
		t.Fatalf("default session plan state = %+v", msgs[0].Plan)
	}
}

func TestBuildAuthorityPlanModeBounds(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}}
	planPath := filepath.Join(root, planfile.DirName, session.ID+".md")
	authority, err := BuildAuthority(ctx, nil, root, request, permission.ModePlan, planPath)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Mode != permission.ModePlan || authority.PlanFilePath != planPath {
		t.Fatalf("plan authority = %+v", authority)
	}
	authority, err = BuildAuthority(ctx, nil, root, request, permission.ModeAcceptEdits, "")
	if err != nil {
		t.Fatal(err)
	}
	if authority.Mode != permission.ModeAcceptEdits || authority.PlanFilePath != "" {
		t.Fatalf("accept-edits authority = %+v", authority)
	}
	authority, err = BuildAuthority(ctx, nil, root, request, permission.ModeDefault, "")
	if err != nil {
		t.Fatal(err)
	}
	if authority.Mode != permission.ModeDefault || authority.PlanFilePath != "" {
		t.Fatalf("default authority = %+v", authority)
	}
	// Goal runs can never carry a plan file path.
	goal := agent.ExecutionRequest{RunID: "run-g", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-1", WorkItemID: "item-1"}}
	if _, err := BuildAuthority(ctx, nil, root, goal, permission.ModePlan, planPath); err == nil {
		t.Fatal("goal run accepted plan authority")
	}
}

// Runs started in plan mode carry the plan reminder as per-turn context after
// the replayed history and before the newest user message: full workflow text
// on the first run, compact text in between, and nothing after an approval.
func TestStartRunPlanReminderInjection(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{ProjectRoot: root, ProviderName: "openai-compatible", Model: "mock"}, clients: map[chan ServerMsg]*clientSubscription{}}
	if _, err := svc.SetPlanMode(session.ID, sessionlog.PlanModePlan, sessionlog.PlanModeReasonUserToggle); err != nil {
		t.Fatal(err)
	}
	planPath := svc.PlanStateOf(session.ID).PlanPath
	// The first run sees no plan file yet: the full reminder asks for creation.
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	updates := make(chan ServerMsg, 8)
	svc.mu.Lock()
	svc.clients[updates] = &clientSubscription{ch: updates}
	svc.mu.Unlock()
	runner := newTerminalRunner(session.ID, "run-1")
	svc.deps.Runner = runner
	first := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "调研", Messages: []llm.Message{{Role: "user", Content: "调研一下"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &first}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages := runner.request.Messages
	if len(messages) != 3 || messages[0].Role != "system" || messages[1].Role != "user" || messages[1].Content != prompt.BuildPlanModeReminder(planPath, false, 1) || messages[2].Role != "user" || messages[2].Content != "调研一下" {
		t.Fatalf("first plan run messages = %+v", messages)
	}
	var authority permission.Authority
	if err := json.Unmarshal(runner.request.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	if authority.Mode != permission.ModePlan || authority.PlanFilePath != planPath {
		t.Fatalf("first plan run authority = %+v", authority)
	} // The reminder is request context only: it must not land in the log.
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(transcript.Events)
	if strings.Contains(string(encoded), "计划模式已激活") {
		t.Fatal("plan reminder leaked into the session log")
	}
	// The agent "wrote" the plan file between runs; the next run gets the
	// compact reminder with the run counter advanced.
	if _, _, err := planfile.Ensure(root, session.ID); err != nil {
		t.Fatal(err)
	}
	runner = newTerminalRunner(session.ID, "run-2")
	svc.deps.Runner = runner
	second := agent.ExecutionRequest{RunID: "run-2", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "继续", Messages: []llm.Message{{Role: "user", Content: "继续"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &second}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages = runner.request.Messages
	if len(messages) != 4 || messages[0].Role != "system" || messages[1].Content != "调研一下" || messages[2].Content != prompt.BuildPlanModeReminder(planPath, true, 2) || messages[3].Content != "继续" {
		t.Fatalf("second plan run messages = %+v", messages)
	}
	if state := svc.PlanStateOf(session.ID); state.Runs != 2 {
		t.Fatalf("plan runs = %d", state.Runs)
	}
	// After an auto approval the session leaves plan mode: the next run uses
	// accept-edits semantics and no reminder.
	if _, err := svc.SubmitPlanApproval(session.ID, "run-2", planPath); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.ResolvePlanApproval(session.ID, PlanResolveAuto, ""); err != nil {
		t.Fatal(err)
	}
	runner = newTerminalRunner(session.ID, "run-3")
	svc.deps.Runner = runner
	third := agent.ExecutionRequest{RunID: "run-3", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "执行", Messages: []llm.Message{{Role: "user", Content: "执行"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &third}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages = runner.request.Messages
	// The approval's ordinary user message is part of the replayed history now.
	if len(messages) != 5 || messages[0].Role != "system" || messages[1].Content != "调研一下" || messages[2].Content != "继续" || messages[3].Content != "计划已批准，后续按自动接受模式执行" || messages[4].Content != "执行" {
		t.Fatalf("post-approval run messages = %+v", messages)
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "计划模式") {
			t.Fatalf("reminder injected after approval: %+v", messages)
		}
	}
	var accepted permission.Authority
	if err := json.Unmarshal(runner.request.PermissionBounds, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Mode != permission.ModeAcceptEdits || accepted.PlanFilePath != "" {
		t.Fatalf("post-approval authority = %+v", accepted)
	}
	// A default session never sees plan bounds or reminders.
	plain, err := sessionlog.Create(root, "plain")
	if err != nil {
		t.Fatal(err)
	}
	runner = newTerminalRunner(plain.ID, "run-4")
	svc.deps.Runner = runner
	fourth := agent.ExecutionRequest{RunID: "run-4", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: plain.ID}, Intent: "查", Messages: []llm.Message{{Role: "user", Content: "查一下"}}}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: plain.ID, Run: &fourth}, updates); err != nil {
		t.Fatal(err)
	}
	waitRunOutcome(t, updates)
	messages = runner.request.Messages
	if len(messages) != 2 || messages[0].Role != "system" || messages[1].Content != "查一下" {
		t.Fatalf("default run messages = %+v", messages)
	}
	var plainAuthority permission.Authority
	if err := json.Unmarshal(runner.request.PermissionBounds, &plainAuthority); err != nil {
		t.Fatal(err)
	}
	if plainAuthority.Mode != permission.ModeDefault || plainAuthority.PlanFilePath != "" {
		t.Fatalf("default authority = %+v", plainAuthority)
	}
}

// The plan approval sink blocks on the submitted request, reports every
// terminal decision in the shape the executor expects, and cancels the
// request when the run context goes away.
func TestPlanApprovalSink(t *testing.T) {
	type sinkResult struct {
		choice string
		err    error
	}
	subscribe := func(t *testing.T, svc *Service, sessionID string) chan ServerMsg {
		t.Helper()
		ch := make(chan ServerMsg, 4)
		svc.mu.Lock()
		svc.clients[ch] = &clientSubscription{ch: ch, sessionID: sessionID}
		svc.mu.Unlock()
		return ch
	}
	waitPending := func(t *testing.T, ch chan ServerMsg) PlanApprovalRef {
		t.Helper()
		select {
		case msg := <-ch:
			if msg.Type != "plan_approval_pending" || len(msg.PlanApprovals) != 1 {
				t.Fatalf("pending broadcast = %+v", msg)
			}
			return msg.PlanApprovals[0]
		case <-time.After(2 * time.Second):
			t.Fatal("no plan approval pending broadcast")
			return PlanApprovalRef{}
		}
	}
	waitSink := func(t *testing.T, results chan sinkResult) sinkResult {
		t.Helper()
		select {
		case r := <-results:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("sink did not return")
			return sinkResult{}
		}
	}
	newSink := func(svc *Service) *PlanApprovalSink {
		sink := NewPlanApprovalSink(svc)
		sink.PollEvery = time.Millisecond
		return sink
	}

	t.Run("auto", func(t *testing.T) {
		svc, _, sessionID := newPlanFixture(t)
		planPath := svc.PlanStateOf(sessionID).PlanPath
		sink := newSink(svc)
		ch := subscribe(t, svc, sessionID)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan sinkResult, 1)
		go func() {
			choice, err := sink.SubmitPlan(ctx, sessionID, "run-1", planPath)
			results <- sinkResult{choice, err}
		}()
		ref := waitPending(t, ch)
		if ref.RunID != "run-1" || ref.PlanPath != planPath || ref.ID == "" {
			t.Fatalf("pending ref = %+v", ref)
		}
		if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveAuto, ""); err != nil {
			t.Fatal(err)
		}
		if r := waitSink(t, results); r.err != nil || r.choice != execution.PlanChoiceAuto {
			t.Fatalf("sink = %q %v", r.choice, r.err)
		}
		select {
		case msg := <-ch:
			if msg.Type != "plan_approval_resolved" || msg.PlanState == nil || msg.PlanState.Mode != sessionlog.PlanModeDefault || msg.PlanState.ExecutionMode != PlanExecutionAcceptEdits {
				t.Fatalf("resolved broadcast = %+v", msg)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no resolved broadcast")
		}
		// With no pending request left, a new submission is accepted.
		if _, err := svc.SubmitPlanApproval(sessionID, "run-2", planPath); err != nil {
			t.Fatalf("resubmit after resolution = %v", err)
		}
	})

	t.Run("manual", func(t *testing.T) {
		svc, _, sessionID := newPlanFixture(t)
		planPath := svc.PlanStateOf(sessionID).PlanPath
		sink := newSink(svc)
		ch := subscribe(t, svc, sessionID)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan sinkResult, 1)
		go func() {
			choice, err := sink.SubmitPlan(ctx, sessionID, "run-1", planPath)
			results <- sinkResult{choice, err}
		}()
		waitPending(t, ch)
		if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveManual, ""); err != nil {
			t.Fatal(err)
		}
		if r := waitSink(t, results); r.err != nil || r.choice != execution.PlanChoiceManual {
			t.Fatalf("sink = %q %v", r.choice, r.err)
		}
	})

	t.Run("feedback", func(t *testing.T) {
		svc, _, sessionID := newPlanFixture(t)
		planPath := svc.PlanStateOf(sessionID).PlanPath
		sink := newSink(svc)
		ch := subscribe(t, svc, sessionID)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan sinkResult, 1)
		go func() {
			choice, err := sink.SubmitPlan(ctx, sessionID, "run-1", planPath)
			results <- sinkResult{choice, err}
		}()
		waitPending(t, ch)
		if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveFeedback, "补验证章节"); err != nil {
			t.Fatal(err)
		}
		r := waitSink(t, results)
		var feedback execution.PlanFeedbackError
		if !errors.As(r.err, &feedback) || feedback.Text != "补验证章节" {
			t.Fatalf("sink error = %v", r.err)
		}
		if state := svc.PlanStateOf(sessionID); state.Mode != sessionlog.PlanModePlan {
			t.Fatalf("state after feedback = %+v", state)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		svc, _, sessionID := newPlanFixture(t)
		planPath := svc.PlanStateOf(sessionID).PlanPath
		sink := newSink(svc)
		ch := subscribe(t, svc, sessionID)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan sinkResult, 1)
		go func() {
			choice, err := sink.SubmitPlan(ctx, sessionID, "run-1", planPath)
			results <- sinkResult{choice, err}
		}()
		waitPending(t, ch)
		if _, _, _, err := svc.ResolvePlanApproval(sessionID, PlanResolveCancel, ""); err != nil {
			t.Fatal(err)
		}
		r := waitSink(t, results)
		var cancelled execution.PlanCancelledError
		if !errors.As(r.err, &cancelled) {
			t.Fatalf("sink error = %v", r.err)
		}
	})

	t.Run("cancelled by context", func(t *testing.T) {
		svc, root, sessionID := newPlanFixture(t)
		planPath := svc.PlanStateOf(sessionID).PlanPath
		sink := newSink(svc)
		ch := subscribe(t, svc, sessionID)
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan sinkResult, 1)
		go func() {
			choice, err := sink.SubmitPlan(ctx, sessionID, "run-1", planPath)
			results <- sinkResult{choice, err}
		}()
		ref := waitPending(t, ch)
		cancel()
		r := waitSink(t, results)
		var cancelled execution.PlanCancelledError
		if !errors.As(r.err, &cancelled) {
			t.Fatalf("sink error = %v", r.err)
		}
		// The dangling request was resolved as cancelled in the log and the
		// session stays in plan mode.
		transcript, err := sessionlog.Replay(root, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		_, records, _ := decodePlanEvents(t, transcript.Events)
		if len(records) != 2 || records[1].RequestID != ref.ID || records[1].Status != sessionlog.PlanApprovalCancelled || records[1].ResolvedAt.IsZero() {
			t.Fatalf("plan approval records = %+v", records)
		}
		if state := svc.PlanStateOf(sessionID); state.Mode != sessionlog.PlanModePlan {
			t.Fatalf("state after context cancel = %+v", state)
		}
	})
}

// The plan_approvals wire payload only exposes the dialog fields.
func TestPlanApprovalRefStripsInternalFields(t *testing.T) {
	raw, err := json.Marshal(PlanApprovalRef{ID: "p-1", SessionID: "s", RunID: "r", PlanPath: "/p.md", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "session_id", "run_id", "plan_path", "created_at"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing key %s in %v", k, keys)
		}
		delete(keys, k)
	}
	if len(keys) != 0 {
		t.Fatalf("unexpected keys: %v", keys)
	}
}
