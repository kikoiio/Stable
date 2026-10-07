package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func startRun(t *testing.T, root, sessionID, runID string) {
	t.Helper()
	if _, err := Append(root, sessionID, EventRunStarted, RunStarted{RunID: runID, WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorModeEventPersistsSessionSetting(t *testing.T) {
	root := t.TempDir()
	session, err := Create(root, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		if _, err := Append(root, session.ID, EventCoordinatorMode, CoordinatorMode{Enabled: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	transcript, err := Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var modes []bool
	for _, event := range transcript.Events {
		if event.Type != EventCoordinatorMode {
			continue
		}
		var mode CoordinatorMode
		if err := decodeData(event.Data, &mode); err != nil {
			t.Fatal(err)
		}
		modes = append(modes, mode.Enabled)
	}
	if len(modes) != 2 || !modes[0] || modes[1] {
		t.Fatalf("coordinator mode event history=%v", modes)
	}
	if _, err := Append(root, session.ID, EventCoordinatorMode, map[string]any{"enabled": true, "forged": true}); err == nil {
		t.Fatal("unknown coordinator mode event field accepted")
	}
}

func addRunEvent(t *testing.T, root, sessionID, runID, eventID string, runSeq uint64) Event {
	t.Helper()
	e, err := Append(root, sessionID, EventRunEvent, RunEvent{
		ID:        eventID,
		RunID:     runID,
		SessionID: sessionID,
		RunSeq:    runSeq,
		At:        time.Now().UTC(),
		Kind:      "text_delta",
		Payload:   map[string]string{"text": eventID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestBoundaryScopeValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "boundary")
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, root, s.ID, "r1")
	addRunEvent(t, root, s.ID, "r1", "e1", 1)
	addRunEvent(t, root, s.ID, "r1", "e2", 2)

	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 2, Summary: "legacy"}); err != nil {
		t.Fatalf("legacy session boundary rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 1, Summary: "run", Scope: BoundaryScopeRun, RunID: "r1"}); err != nil {
		t.Fatalf("run scope boundary rejected: %v", err)
	}
	cases := []struct {
		name string
		b    Boundary
	}{
		{"bad scope", Boundary{FromSeq: 1, ToSeq: 2, Summary: "x", Scope: "week"}},
		{"run scope missing run", Boundary{FromSeq: 1, ToSeq: 2, Summary: "x", Scope: BoundaryScopeRun}},
		{"session scope names run", Boundary{FromSeq: 1, ToSeq: 2, Summary: "x", Scope: BoundaryScopeSession, RunID: "r1"}},
		{"unknown run", Boundary{FromSeq: 1, ToSeq: 1, Summary: "x", Scope: BoundaryScopeRun, RunID: "nope"}},
		{"range beyond run", Boundary{FromSeq: 1, ToSeq: 9, Summary: "x", Scope: BoundaryScopeRun, RunID: "r1"}},
		{"empty summary", Boundary{FromSeq: 1, ToSeq: 2}},
	}
	for _, tc := range cases {
		if _, err = Append(root, s.ID, EventBoundary, tc.b); err == nil {
			t.Fatalf("%s: invalid boundary accepted", tc.name)
		}
	}
	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var legacy, runScope Boundary
	if err = decodeData(replayed.Events[4].Data, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.EffectiveScope() != BoundaryScopeSession {
		t.Fatalf("legacy scope = %q", legacy.EffectiveScope())
	}
	if err = decodeData(replayed.Events[5].Data, &runScope); err != nil {
		t.Fatal(err)
	}
	if runScope.EffectiveScope() != BoundaryScopeRun || runScope.RunID != "r1" {
		t.Fatalf("run boundary = %+v", runScope)
	}
}

func TestBoundaryCannotCoverItselfOrFuture(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "future")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	// The boundary will land at seq 3; covering seq 3 or beyond is invalid.
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 3, Summary: "self"}); err == nil {
		t.Fatal("boundary covering itself accepted")
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 2, Summary: "ok"}); err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}
}

func TestBoundaryCannotSplitToolPair(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "pairs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolCall, ToolCall{CallID: "c1", Name: "write"}); err != nil {
		t.Fatal(err)
	}
	// Tool call at seq 3 is still open; a boundary covering it splits the pair.
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 3, Summary: "split"}); err == nil {
		t.Fatal("boundary splitting open tool pair accepted")
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 2, Summary: "before call"}); err != nil {
		t.Fatalf("boundary before tool call rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventToolResult, ToolResult{CallID: "c1", Result: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 5, Summary: "complete pair"}); err != nil {
		t.Fatalf("boundary over completed pair rejected: %v", err)
	}
}

func TestReplayRejectsPairSplittingBoundary(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "corrupt-boundary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolCall, ToolCall{CallID: "c1", Name: "write"}); err != nil {
		t.Fatal(err)
	}
	// Bypass append validation to simulate a corrupted or downgraded writer.
	raw, err := json.Marshal(Event{
		SchemaVersion: SchemaVersion,
		SessionID:     s.ID,
		Seq:           3,
		At:            time.Now().UTC(),
		Type:          EventBoundary,
		Data:          Boundary{FromSeq: 1, ToSeq: 2, Summary: "split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := SessionPath(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Replay(root, s.ID); err == nil || !strings.Contains(err.Error(), "splits a tool call") {
		t.Fatalf("replay accepted pair-splitting boundary: %v", err)
	}
}

func TestSnapshotAndRewindValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, root, s.ID, "r1")
	now := time.Now().UTC()

	snap := SnapshotRef{SnapshotID: "snap-1", SessionID: s.ID, CandidateID: "cand-1", RunID: "r1", Digest: "sha256:abc", CreatedAt: now}
	if _, err = Append(root, s.ID, EventSnapshot, snap); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	bad := []struct {
		name string
		snap SnapshotRef
	}{
		{"wrong session", SnapshotRef{SnapshotID: "snap-2", SessionID: "other", CandidateID: "cand-1", Digest: "d", CreatedAt: now}},
		{"missing digest", SnapshotRef{SnapshotID: "snap-2", SessionID: s.ID, CandidateID: "cand-1", CreatedAt: now}},
		{"unknown run", SnapshotRef{SnapshotID: "snap-2", SessionID: s.ID, CandidateID: "cand-1", RunID: "nope", Digest: "d", CreatedAt: now}},
		{"duplicate id", snap},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, EventSnapshot, tc.snap); err == nil {
			t.Fatalf("%s: invalid snapshot accepted", tc.name)
		}
	}

	if _, err = Append(root, s.ID, EventRewind, RewindRecord{SnapshotID: "snap-1", CandidateID: "cand-1", Status: RewindCompleted, CreatedAt: now}); err != nil {
		t.Fatalf("valid rewind rejected: %v", err)
	}
	rewindBad := []struct {
		name   string
		rewind RewindRecord
	}{
		{"unknown snapshot", RewindRecord{SnapshotID: "snap-9", CandidateID: "cand-1", Status: RewindCompleted, CreatedAt: now}},
		{"candidate mismatch", RewindRecord{SnapshotID: "snap-1", CandidateID: "cand-2", Status: RewindCompleted, CreatedAt: now}},
		{"bad status", RewindRecord{SnapshotID: "snap-1", CandidateID: "cand-1", Status: "maybe", CreatedAt: now}},
		{"missing time", RewindRecord{SnapshotID: "snap-1", CandidateID: "cand-1", Status: RewindFailed}},
	}
	for _, tc := range rewindBad {
		if _, err = Append(root, s.ID, EventRewind, tc.rewind); err == nil {
			t.Fatalf("%s: invalid rewind accepted", tc.name)
		}
	}
	if _, err = Replay(root, s.ID); err != nil {
		t.Fatalf("replay of valid snapshot/rewind log: %v", err)
	}
}

func TestQuestionAndReplyValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "questions")
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, root, s.ID, "r1")
	now := time.Now().UTC()

	question := PendingQuestion{
		QuestionID: "q1",
		WorkRef:    "goal:g1/work:w1",
		SessionID:  s.ID,
		RunID:      "r1",
		Prompt:     "continue?",
		CreatedAt:  now,
		Status:     QuestionPending,
	}
	if _, err = Append(root, s.ID, EventQuestion, question); err != nil {
		t.Fatalf("valid question rejected: %v", err)
	}
	bad := []struct {
		name string
		q    PendingQuestion
	}{
		{"duplicate id", question},
		{"not pending", PendingQuestion{QuestionID: "q2", WorkRef: "w", SessionID: s.ID, RunID: "r1", Prompt: "p", CreatedAt: now, Status: QuestionReplied}},
		{"unknown run", PendingQuestion{QuestionID: "q2", WorkRef: "w", SessionID: s.ID, RunID: "nope", Prompt: "p", CreatedAt: now, Status: QuestionPending}},
		{"wrong session", PendingQuestion{QuestionID: "q2", WorkRef: "w", SessionID: "other", RunID: "r1", Prompt: "p", CreatedAt: now, Status: QuestionPending}},
		{"missing workref", PendingQuestion{QuestionID: "q2", SessionID: s.ID, RunID: "r1", Prompt: "p", CreatedAt: now, Status: QuestionPending}},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, EventQuestion, tc.q); err == nil {
			t.Fatalf("%s: invalid question accepted", tc.name)
		}
	}

	if _, err = Append(root, s.ID, EventReply, QuestionReply{QuestionID: "q9", ReplyText: "x", RepliedAt: now}); err == nil {
		t.Fatal("reply to unknown question accepted")
	}
	if _, err = Append(root, s.ID, EventReply, QuestionReply{QuestionID: "q1", ReplyText: "yes", RepliedAt: now}); err != nil {
		t.Fatalf("valid reply rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventReply, QuestionReply{QuestionID: "q1", ReplyText: "again", RepliedAt: now}); err == nil {
		t.Fatal("second reply to answered question accepted")
	}
	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay of question log: %v", err)
	}
	var got PendingQuestion
	if err = decodeData(replayed.Events[2].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.QuestionID != "q1" || got.WorkRef != "goal:g1/work:w1" || got.Status != QuestionPending {
		t.Fatalf("question round-trip = %+v", got)
	}
}

func TestPlanModeValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "plan-mode")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	if _, err = Append(root, s.ID, EventPlanMode, PlanMode{Mode: PlanModePlan, Reason: PlanModeReasonUserToggle, At: now}); err != nil {
		t.Fatalf("valid plan_mode rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventPlanMode, PlanMode{Mode: PlanModeDefault, Reason: PlanModeReasonPlanCancelled, At: now}); err != nil {
		t.Fatalf("valid plan_mode toggle back rejected: %v", err)
	}
	bad := []struct {
		name string
		m    PlanMode
	}{
		{"invalid mode", PlanMode{Mode: "auto", Reason: PlanModeReasonUserToggle, At: now}},
		{"invalid reason", PlanMode{Mode: PlanModePlan, Reason: "because", At: now}},
		{"missing time", PlanMode{Mode: PlanModePlan, Reason: PlanModeReasonUserToggle}},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, EventPlanMode, tc.m); err == nil {
			t.Fatalf("%s: invalid plan_mode accepted", tc.name)
		}
	}
	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay of plan_mode log: %v", err)
	}
	var got PlanMode
	if err = decodeData(replayed.Events[1].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != PlanModePlan || got.Reason != PlanModeReasonUserToggle {
		t.Fatalf("plan_mode round-trip = %+v", got)
	}
}

func TestPlanApprovalValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "plan-approval")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	path := "/proj/.stable/plans/session.md"

	submitted := PlanApprovalRecord{RequestID: "pa-1", RunID: "r1", PlanPath: path, Status: PlanApprovalSubmitted, CreatedAt: now}
	if _, err = Append(root, s.ID, EventPlanApproval, submitted); err != nil {
		t.Fatalf("valid submitted rejected: %v", err)
	}
	resolved := submitted
	resolved.Status = PlanApprovalApprovedAuto
	resolved.ResolvedAt = now
	bad := []struct {
		name string
		r    PlanApprovalRecord
	}{
		{"duplicate submitted", submitted},
		{"terminal without submitted", PlanApprovalRecord{RequestID: "pa-2", RunID: "r1", PlanPath: path, Status: PlanApprovalApprovedManual, CreatedAt: now, ResolvedAt: now}},
		{"invalid status", PlanApprovalRecord{RequestID: "pa-2", RunID: "r1", PlanPath: path, Status: "maybe", CreatedAt: now}},
		{"missing request id", PlanApprovalRecord{RunID: "r1", PlanPath: path, Status: PlanApprovalSubmitted, CreatedAt: now}},
		{"missing run id", PlanApprovalRecord{RequestID: "pa-2", PlanPath: path, Status: PlanApprovalSubmitted, CreatedAt: now}},
		{"missing plan path", PlanApprovalRecord{RequestID: "pa-2", RunID: "r1", Status: PlanApprovalSubmitted, CreatedAt: now}},
		{"missing created at", PlanApprovalRecord{RequestID: "pa-2", RunID: "r1", PlanPath: path, Status: PlanApprovalSubmitted}},
		{"submitted with resolution time", PlanApprovalRecord{RequestID: "pa-2", RunID: "r1", PlanPath: path, Status: PlanApprovalSubmitted, CreatedAt: now, ResolvedAt: now}},
		{"terminal without resolution time", PlanApprovalRecord{RequestID: "pa-1", RunID: "r1", PlanPath: path, Status: PlanApprovalApprovedAuto, CreatedAt: now}},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, EventPlanApproval, tc.r); err == nil {
			t.Fatalf("%s: invalid plan_approval accepted", tc.name)
		}
	}

	if _, err = Append(root, s.ID, EventPlanApproval, resolved); err != nil {
		t.Fatalf("valid submitted->approved_auto rejected: %v", err)
	}
	after := []struct {
		name string
		r    PlanApprovalRecord
	}{
		{"second terminal after terminal", PlanApprovalRecord{RequestID: "pa-1", RunID: "r1", PlanPath: path, Status: PlanApprovalCancelled, CreatedAt: now, ResolvedAt: now}},
		{"resubmit after terminal", submitted},
	}
	for _, tc := range after {
		if _, err = Append(root, s.ID, EventPlanApproval, tc.r); err == nil {
			t.Fatalf("%s: invalid plan_approval accepted", tc.name)
		}
	}

	// A second request runs the full lifecycle including a feedback terminal.
	feedbackSubmitted := PlanApprovalRecord{RequestID: "pa-3", RunID: "r1", PlanPath: path, Status: PlanApprovalSubmitted, CreatedAt: now}
	if _, err = Append(root, s.ID, EventPlanApproval, feedbackSubmitted); err != nil {
		t.Fatalf("valid second submitted rejected: %v", err)
	}
	feedback := feedbackSubmitted
	feedback.Status = PlanApprovalFeedback
	feedback.Feedback = "add a rollback step"
	feedback.ResolvedAt = now
	if _, err = Append(root, s.ID, EventPlanApproval, feedback); err != nil {
		t.Fatalf("valid feedback terminal rejected: %v", err)
	}

	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay of plan_approval log: %v", err)
	}
	var got PlanApprovalRecord
	if err = decodeData(replayed.Events[2].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "pa-1" || got.Status != PlanApprovalApprovedAuto || got.ResolvedAt.IsZero() {
		t.Fatalf("plan_approval round-trip = %+v", got)
	}
	var gotFeedback PlanApprovalRecord
	if err = decodeData(replayed.Events[4].Data, &gotFeedback); err != nil {
		t.Fatal(err)
	}
	if gotFeedback.RequestID != "pa-3" || gotFeedback.Status != PlanApprovalFeedback || gotFeedback.Feedback == "" {
		t.Fatalf("feedback round-trip = %+v", gotFeedback)
	}
}

func TestTodoUpdateValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "todo")
	if err != nil {
		t.Fatal(err)
	}

	task := TaskSnapshot{ID: "t1", Subject: "Explore", ActiveForm: "Exploring", Status: "in_progress"}
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 2, Tasks: []TaskSnapshot{task}}); err != nil {
		t.Fatalf("valid first todo_update rejected: %v", err)
	}
	bad := []struct {
		name string
		u    TodoUpdate
	}{
		{"equal revision", TodoUpdate{Revision: 2, Tasks: []TaskSnapshot{task}}},
		{"lower revision", TodoUpdate{Revision: 1, Tasks: []TaskSnapshot{task}}},
		{"zero revision", TodoUpdate{Revision: 0, Tasks: []TaskSnapshot{task}}},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, EventTodo, tc.u); err == nil {
			t.Fatalf("%s: invalid todo_update accepted", tc.name)
		}
	}
	// Strictly increasing does not require consecutive revisions.
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 5, Tasks: []TaskSnapshot{task}}); err != nil {
		t.Fatalf("revision gap rejected: %v", err)
	}

	tooMany := make([]TaskSnapshot, MaxTodoTasks+1)
	for i := range tooMany {
		tooMany[i] = TaskSnapshot{ID: fmt.Sprintf("t%d", i), Subject: "x", Status: "pending"}
	}
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 6, Tasks: tooMany}); err == nil {
		t.Fatal("todo_update over the task limit accepted")
	}
	exact := tooMany[:MaxTodoTasks]
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 6, Tasks: exact}); err != nil {
		t.Fatalf("todo_update at the task limit rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 7, Tasks: []TaskSnapshot{}}); err != nil {
		t.Fatalf("empty todo_update (clear) rejected: %v", err)
	}

	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay of todo log: %v", err)
	}
	var got TodoUpdate
	if err = decodeData(replayed.Events[1].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || len(got.Tasks) != 1 || got.Tasks[0].ID != "t1" || got.Tasks[0].ActiveForm != "Exploring" {
		t.Fatalf("todo round-trip = %+v", got)
	}
}

func TestSkillEventValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "skills")
	if err != nil {
		t.Fatal(err)
	}

	overflow := make([]SkillInfo, MaxSkillListEntries+1)
	for i := range overflow {
		overflow[i] = SkillInfo{Name: fmt.Sprintf("s%d", i), Description: "d", Source: "user"}
	}
	bad := []struct {
		name string
		typ  string
		data any
	}{
		{"inventory missing name", EventSkillInventory, SkillInventory{Skills: []SkillInfo{{Description: "d", Source: "user"}}}},
		{"inventory over limit", EventSkillInventory, SkillInventory{Skills: overflow}},
		{"delta missing name", EventSkillDelta, SkillDelta{Added: []SkillInfo{{Description: "d", Source: "project"}}}},
		{"delta over limit", EventSkillDelta, SkillDelta{Added: overflow}},
		{"invoked missing name", EventSkillInvoked, SkillInvoked{Entry: SkillEntrySlash}},
		{"invoked missing entry", EventSkillInvoked, SkillInvoked{Name: "code-review"}},
		{"invoked invalid entry", EventSkillInvoked, SkillInvoked{Name: "code-review", Entry: "auto"}},
		{"invoked invalid mode", EventSkillInvoked, SkillInvoked{Name: "code-review", Entry: SkillEntrySlash, Mode: "background"}},
		{"fork invocation missing run id", EventSkillInvoked, SkillInvoked{Name: "code-review", Entry: SkillEntrySlash, Mode: SkillModeFork}},
		{"run id without fork mode", EventSkillInvoked, SkillInvoked{Name: "code-review", Entry: SkillEntrySlash, RunID: "fork-1"}},
	}
	for _, tc := range bad {
		if _, err = Append(root, s.ID, tc.typ, tc.data); err == nil {
			t.Fatalf("%s: invalid skill event accepted", tc.name)
		}
	}

	info := SkillInfo{Name: "code-review", Description: "Review the diff", WhenToUse: "before commits", Source: "user"}
	if _, err = Append(root, s.ID, EventSkillInventory, SkillInventory{Skills: []SkillInfo{info}}); err != nil {
		t.Fatalf("valid skill_inventory rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillInventory, SkillInventory{Skills: []SkillInfo{{Name: "other", Source: "user"}}}); err == nil {
		t.Fatal("second skill_inventory accepted")
	}
	if _, err = Append(root, s.ID, EventSkillDelta, SkillDelta{Added: []SkillInfo{{Name: "fresh-skill", Description: "New", Source: "project"}}}); err != nil {
		t.Fatalf("valid skill_delta rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillDelta, SkillDelta{Added: []SkillInfo{}}); err != nil {
		t.Fatalf("empty skill_delta rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillInvoked, SkillInvoked{Name: "code-review", Source: "user", Entry: SkillEntrySlash, Args: "focus on memory"}); err != nil {
		t.Fatalf("valid slash skill_invoked rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillInvoked, SkillInvoked{Name: "fresh-skill", Source: "project", Entry: SkillEntryTool}); err != nil {
		t.Fatalf("valid tool skill_invoked rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillInvoked, SkillInvoked{Name: "code-review", Source: "user", Entry: SkillEntryTool}); err != nil {
		t.Fatalf("repeated skill invocation rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{
		RunID: "fork-1", WorkKind: "session", Intent: "run skill",
		ForkSkill: "code-review", ForkEntry: SkillEntrySlash,
	}); err != nil {
		t.Fatalf("fork run start rejected: %v", err)
	}
	if _, err = Append(root, s.ID, EventSkillInvoked, SkillInvoked{
		Name: "code-review", Source: "user", Entry: SkillEntrySlash,
		Mode: SkillModeFork, RunID: "fork-1",
	}); err != nil {
		t.Fatalf("valid fork skill_invoked rejected: %v", err)
	}

	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay of skill log: %v", err)
	}
	var inv SkillInventory
	if err = decodeData(replayed.Events[1].Data, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Skills) != 1 || inv.Skills[0].Name != "code-review" || inv.Skills[0].WhenToUse != "before commits" || inv.Skills[0].Source != "user" {
		t.Fatalf("inventory round-trip = %+v", inv)
	}
	var delta SkillDelta
	if err = decodeData(replayed.Events[2].Data, &delta); err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 1 || delta.Added[0].Name != "fresh-skill" {
		t.Fatalf("delta round-trip = %+v", delta)
	}
	var invoked SkillInvoked
	if err = decodeData(replayed.Events[4].Data, &invoked); err != nil {
		t.Fatal(err)
	}
	if invoked.Name != "code-review" || invoked.Entry != SkillEntrySlash || invoked.Args != "focus on memory" {
		t.Fatalf("invoked round-trip = %+v", invoked)
	}
	var forkInvoked SkillInvoked
	if err = decodeData(replayed.Events[len(replayed.Events)-1].Data, &forkInvoked); err != nil {
		t.Fatal(err)
	}
	if forkInvoked.Mode != SkillModeFork || forkInvoked.RunID != "fork-1" {
		t.Fatalf("fork invocation metadata round-trip = %+v", forkInvoked)
	}
	var forkRun RunStarted
	if err = decodeData(replayed.Events[len(replayed.Events)-2].Data, &forkRun); err != nil {
		t.Fatal(err)
	}
	if forkRun.ForkSkill != "code-review" || forkRun.ForkEntry != SkillEntrySlash {
		t.Fatalf("fork run metadata round-trip = %+v", forkRun)
	}
}

func TestReplayRejectsDuplicateSkillInventory(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "corrupt-skills")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventSkillInventory, SkillInventory{Skills: []SkillInfo{{Name: "a", Source: "user"}}}); err != nil {
		t.Fatal(err)
	}
	// Bypass append validation to simulate a corrupted or downgraded writer.
	raw, err := json.Marshal(Event{
		SchemaVersion: SchemaVersion,
		SessionID:     s.ID,
		Seq:           3,
		At:            time.Now().UTC(),
		Type:          EventSkillInventory,
		Data:          SkillInventory{Skills: []SkillInfo{{Name: "b", Source: "user"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := SessionPath(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Replay(root, s.ID); err == nil || !strings.Contains(err.Error(), "already has a skill inventory") {
		t.Fatalf("replay accepted duplicate skill inventory: %v", err)
	}
}
