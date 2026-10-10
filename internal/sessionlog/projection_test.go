package sessionlog

import (
	"testing"
	"time"
)

func kinds(p Projection) []ItemKind {
	out := make([]ItemKind, 0, len(p.Items))
	for _, item := range p.Items {
		out = append(out, item.Kind)
	}
	return out
}

func TestProjectionSubstitutesLatestBoundary(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "assistant", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 2, ToSeq: 3, Summary: "early summary"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "tail"}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemSummary, ItemMessage}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if p.Items[0].Summary == nil || p.Items[0].Summary.Summary != "early summary" {
		t.Fatalf("summary item = %+v", p.Items[0])
	}
	if p.Items[1].Message == nil || p.Items[1].Message.Text != "tail" {
		t.Fatalf("tail item = %+v", p.Items[1])
	}
	// Raw events stay in place for audit.
	if len(replay.Events) != 5 {
		t.Fatalf("raw events = %d, want 5", len(replay.Events))
	}
}

func TestProjectionRunScopeBoundary(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "run-scope")
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, root, s.ID, "r1")
	addRunEvent(t, root, s.ID, "r1", "e1", 1)
	addRunEvent(t, root, s.ID, "r1", "e2", 2)
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 1, ToSeq: 2, Summary: "run summary", Scope: BoundaryScopeRun, RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "after"}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemSummary, ItemMessage}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestProjectionLegacyScopelessBoundary(t *testing.T) {
	// Logs written before M05 carry boundaries from the old prompt.Compact:
	// no scope, and the retained tail re-appended after the boundary with
	// fresh sequence numbers. The summary must replace everything before the
	// boundary so the original tail is not shown next to its re-appended
	// copies.
	root := t.TempDir()
	s, err := Create(root, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "old question"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "assistant", Text: "old answer"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "kept question"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventBoundary, Boundary{FromSeq: 2, ToSeq: 3, Summary: "legacy summary"}); err != nil {
		t.Fatal(err)
	}
	// The old compactor re-appended the retained tail after the boundary.
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "kept question"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "assistant", Text: "kept answer"}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemSummary, ItemMessage, ItemMessage}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if p.Items[0].Summary == nil || p.Items[0].Summary.Summary != "legacy summary" {
		t.Fatalf("summary item = %+v", p.Items[0])
	}
	if p.Items[1].Message == nil || p.Items[1].Message.Text != "kept question" {
		t.Fatalf("re-appended tail = %+v", p.Items[1])
	}
	if p.Items[2].Message == nil || p.Items[2].Message.Text != "kept answer" {
		t.Fatalf("post-boundary message = %+v", p.Items[2])
	}
}

func TestProjectionToolPairMatching(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "pairs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolCall, ToolCall{CallID: "c1", Name: "write"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolResult, ToolResult{CallID: "c1", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolCall, ToolCall{CallID: "c2", Name: "edit"}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	var calls []Item
	for _, item := range p.Items {
		if item.Kind == ItemToolCall {
			calls = append(calls, item)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	if !calls[0].Matched {
		t.Fatal("completed call marked unmatched")
	}
	// The trailing unmatched call stays visible for audit but is flagged so
	// it cannot become the recoverable tail of a normal conversation.
	if calls[1].Matched {
		t.Fatal("dangling call marked matched")
	}
}

func TestProjectionKeepsAuditEvents(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "audit")
	if err != nil {
		t.Fatal(err)
	}
	startRun(t, root, s.ID, "r1")
	now := time.Now().UTC()
	if _, err = Append(root, s.ID, EventSnapshot, SnapshotRef{SnapshotID: "snap-1", SessionID: s.ID, CandidateID: "cand-1", RunID: "r1", Digest: "d", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRewind, RewindRecord{SnapshotID: "snap-1", CandidateID: "cand-1", Status: RewindCompleted, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventQuestion, PendingQuestion{QuestionID: "q1", WorkRef: "w", SessionID: s.ID, RunID: "r1", Prompt: "ok?", CreatedAt: now, Status: QuestionPending}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventReply, QuestionReply{QuestionID: "q1", ReplyText: "yes", RepliedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "e9", RunID: "r1", SessionID: s.ID, RunSeq: 1, At: now, Kind: "terminal", Payload: map[string]string{"status": "completed"}}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemSnapshot, ItemRewind, ItemQuestion, ItemReply, ItemRunTerminal}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

func TestProjectionPlanAndTodoItems(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "plan-todo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = Append(root, s.ID, EventPlanMode, PlanMode{Mode: PlanModePlan, Reason: PlanModeReasonUserToggle, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventPlanApproval, PlanApprovalRecord{RequestID: "pa-1", RunID: "r1", PlanPath: "/p/.stable/plans/s.md", Status: PlanApprovalSubmitted, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventPlanApproval, PlanApprovalRecord{RequestID: "pa-1", RunID: "r1", PlanPath: "/p/.stable/plans/s.md", Status: PlanApprovalApprovedAuto, CreatedAt: now, ResolvedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventTodo, TodoUpdate{Revision: 1, Tasks: []TaskSnapshot{{ID: "t1", Subject: "Write plan", Status: "in_progress"}}}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemPlanMode, ItemPlanApproval, ItemPlanApproval, ItemTodo}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if p.Items[0].PlanMode == nil || p.Items[0].PlanMode.Mode != PlanModePlan || p.Items[0].PlanMode.Reason != PlanModeReasonUserToggle {
		t.Fatalf("plan mode item = %+v", p.Items[0])
	}
	if p.Items[1].Approval == nil || p.Items[1].Approval.RequestID != "pa-1" || p.Items[1].Approval.Status != PlanApprovalSubmitted {
		t.Fatalf("submitted approval item = %+v", p.Items[1])
	}
	if p.Items[2].Approval == nil || p.Items[2].Approval.Status != PlanApprovalApprovedAuto || p.Items[2].Approval.ResolvedAt.IsZero() {
		t.Fatalf("resolved approval item = %+v", p.Items[2])
	}
	if p.Items[3].Todo == nil || p.Items[3].Todo.Revision != 1 || len(p.Items[3].Todo.Tasks) != 1 || p.Items[3].Todo.Tasks[0].ID != "t1" {
		t.Fatalf("todo item = %+v", p.Items[3])
	}
}

func TestProjectionSkillItems(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "skill-projection")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventSkillInventory, SkillInventory{Skills: []SkillInfo{
		{Name: "code-review", Description: "Review the diff", WhenToUse: "before commits", Source: "user"},
		{Name: "triage", Description: "Triage bugs", WhenToUse: "new issue", Source: "project"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventSkillDelta, SkillDelta{Added: []SkillInfo{{Name: "fresh-skill", Description: "New", WhenToUse: "always", Source: "project"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{
		RunID: "fork-run-1", WorkKind: "session", Intent: "run skill",
		ForkSkill: "code-review", ForkEntry: SkillEntrySlash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventSkillInvoked, SkillInvoked{
		Name: "code-review", Source: "user", Entry: SkillEntrySlash, Args: "focus",
		Mode: SkillModeFork, RunID: "fork-run-1",
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Project(replay)
	got := kinds(p)
	want := []ItemKind{ItemSkillInventory, ItemSkillDelta, ItemSkillInvoked}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	if p.Items[0].SkillInventory == nil || len(p.Items[0].SkillInventory.Skills) != 2 ||
		p.Items[0].SkillInventory.Skills[0].Name != "code-review" ||
		p.Items[0].SkillInventory.Skills[0].Description != "Review the diff" ||
		p.Items[0].SkillInventory.Skills[0].WhenToUse != "before commits" ||
		p.Items[0].SkillInventory.Skills[0].Source != "user" ||
		p.Items[0].SkillInventory.Skills[1].Name != "triage" {
		t.Fatalf("inventory item = %+v", p.Items[0])
	}
	if p.Items[1].SkillDelta == nil || len(p.Items[1].SkillDelta.Added) != 1 || p.Items[1].SkillDelta.Added[0].Name != "fresh-skill" || p.Items[1].SkillDelta.Added[0].Source != "project" {
		t.Fatalf("delta item = %+v", p.Items[1])
	}
	if p.Items[2].SkillInvoked == nil || p.Items[2].SkillInvoked.Name != "code-review" ||
		p.Items[2].SkillInvoked.Source != "user" ||
		p.Items[2].SkillInvoked.Entry != SkillEntrySlash ||
		p.Items[2].SkillInvoked.Args != "focus" ||
		p.Items[2].SkillInvoked.Mode != SkillModeFork ||
		p.Items[2].SkillInvoked.RunID != "fork-run-1" {
		t.Fatalf("invoked item = %+v", p.Items[2])
	}
}
