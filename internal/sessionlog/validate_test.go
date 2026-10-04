package sessionlog

import (
	"encoding/json"
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
