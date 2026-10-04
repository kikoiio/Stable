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
