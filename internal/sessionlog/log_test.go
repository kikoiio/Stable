package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateAppendReplayAndPrivateFiles(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	r, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Session.ID != s.ID || len(r.Events) != 2 || r.Events[1].Seq != 2 {
		t.Fatalf("replay: %+v", r)
	}
	dir, _ := SessionDir(root)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %o", st.Mode().Perm())
	}
	path, _ := SessionPath(root, s.ID)
	st, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("file mode %o", st.Mode().Perm())
	}
	listed, err := List(root)
	if err != nil || len(listed) != 1 || listed[0].ID != s.ID {
		t.Fatalf("list: %+v %v", listed, err)
	}
}

func TestReplayReportsCorruptionWithoutMutation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := SessionPath(root, s.ID)
	if err = os.WriteFile(path, []byte("{broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err = Replay(root, s.ID); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corruption error, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("replay modified source log")
	}
}

func TestReplayReportsIncompleteFinalLineWithoutMutation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := SessionPath(root, s.ID)
	if _, err = Append(root, s.ID, EventMessage, Message{Role: "user", Text: "complete"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"partial":`)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err = Replay(root, s.ID); err == nil || !strings.Contains(err.Error(), "incomplete final line") {
		t.Fatalf("expected incomplete-line error, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("replay mutated incomplete source")
	}
}

func TestToolResultsMustMatchCall(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolResult, ToolResult{CallID: "call-1", Result: "x"}); err == nil {
		t.Fatal("orphan result accepted")
	}
	if _, err = Append(root, s.ID, EventToolCall, ToolCall{CallID: "call-1", Name: "future-tool"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventToolResult, ToolResult{CallID: "call-1", Result: "serialized only"}); err != nil {
		t.Fatal(err)
	}
	r, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var call ToolCall
	raw, _ := json.Marshal(r.Events[1].Data)
	if json.Unmarshal(raw, &call) != nil || call.CallID != "call-1" {
		t.Fatalf("call not recoverable: %+v", r.Events[1])
	}
}

func TestSessionIDAndSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := SessionPath(root, "../escape"); err == nil {
		t.Fatal("traversal id accepted")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".stable")); err != nil {
		t.Skip(err)
	}
	if _, err := Create(root, ""); err == nil {
		t.Fatal("symlinked project state accepted")
	}
}

func TestListSortsByRecentActivity(t *testing.T) {
	root := t.TempDir()
	first, err := Create(root, "first")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := Create(root, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, first.ID, EventActivity, map[string]string{"action": "recent"}); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("recent order: %+v", got)
	}
}

func TestRunEventsEnforcePerRunSequenceAndReplayCursor(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "stream")
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{RunID: "r1", WorkKind: "session", Intent: "hello"}); err != nil {
		t.Fatal(err)
	}
	first, err := Append(root, s.ID, EventRunEvent, RunEvent{ID: "e1", RunID: "r1", SessionID: s.ID, RunSeq: 1, At: startedAt, Kind: "text_delta", Payload: map[string]string{"text": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{RunID: "r2", WorkKind: "goal", GoalID: "g1", WorkItemID: "w1", Intent: "goal work"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "e2", RunID: "r2", SessionID: s.ID, RunSeq: 1, At: startedAt.Add(time.Millisecond), Kind: "text_delta", Payload: map[string]string{"text": "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "e3", RunID: "r1", SessionID: s.ID, RunSeq: 2, At: startedAt.Add(2 * time.Millisecond), Kind: "terminal", Payload: map[string]string{"status": "completed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "e4", RunID: "r1", SessionID: s.ID, RunSeq: 3, At: startedAt.Add(3 * time.Millisecond), Kind: "text_delta"}); err == nil {
		t.Fatal("event after terminal accepted")
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "e5", RunID: "r2", SessionID: s.ID, RunSeq: 3, At: startedAt.Add(4 * time.Millisecond), Kind: "text_delta"}); err == nil {
		t.Fatal("run sequence gap accepted")
	}
	partial, err := ReplayAfter(root, s.ID, first.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.Events) != 3 || partial.Events[0].Seq != first.Seq+1 {
		t.Fatalf("cursor replay=%+v", partial.Events)
	}
	full, err := Replay(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 6 {
		t.Fatalf("full event count=%d", len(full.Events))
	}
}

func TestRunEventBudgetTerminalAppendAndReplay(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{RunID: "r1", WorkKind: "session", Intent: "budgeted work"}); err != nil {
		t.Fatal(err)
	}
	terminal := RunEvent{
		ID:        "e1",
		RunID:     "r1",
		SessionID: s.ID,
		RunSeq:    1,
		At:        time.Now().UTC(),
		Kind:      "terminal",
		Payload:   map[string]string{"status": "budget_exhausted"},
	}
	if _, err = Append(root, s.ID, EventRunEvent, terminal); err != nil {
		t.Fatalf("append budget terminal: %v", err)
	}
	replayed, err := Replay(root, s.ID)
	if err != nil {
		t.Fatalf("replay budget terminal: %v", err)
	}
	if len(replayed.Events) != 3 {
		t.Fatalf("replayed event count = %d, want 3", len(replayed.Events))
	}
	var got RunEvent
	if err = decodeData(replayed.Events[2].Data, &got); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err = decodeData(got.Payload, &payload); err != nil || payload.Status != "budget_exhausted" {
		t.Fatalf("replayed terminal payload = %+v, want budget_exhausted", got.Payload)
	}
}

func TestRunEventTerminalStatusValidation(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "invalid-status")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{RunID: "r1", WorkKind: "session", Intent: "validate status"}); err != nil {
		t.Fatal(err)
	}
	invalid := RunEvent{
		ID:        "e1",
		RunID:     "r1",
		SessionID: s.ID,
		RunSeq:    1,
		At:        time.Now().UTC(),
		Kind:      "terminal",
		Payload:   map[string]string{"status": "not_a_terminal_status"},
	}
	if _, err = Append(root, s.ID, EventRunEvent, invalid); err == nil {
		t.Fatal("append accepted invalid terminal status")
	}

	path, err := SessionPath(root, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{
		ID:        "e1",
		RunID:     "r1",
		SessionID: s.ID,
		RunSeq:    1,
		At:        time.Now().UTC(),
		Kind:      "terminal",
		Payload:   map[string]string{"status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := strings.Replace(string(raw), `"status":"completed"`, `"status":"not_a_terminal_status"`, 1)
	if corrupted == string(raw) {
		t.Fatal("failed to corrupt terminal status")
	}
	if err = os.WriteFile(path, []byte(corrupted), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Replay(root, s.ID); err == nil || !strings.Contains(err.Error(), "invalid terminal status") {
		t.Fatalf("replay accepted invalid terminal status: %v", err)
	}
}

func TestRunEventsRejectMissingStartOrWrongSession(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "stream")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "orphan", RunID: "missing", SessionID: s.ID, RunSeq: 1, At: time.Now().UTC(), Kind: "text_delta"}); err == nil {
		t.Fatal("orphan run event accepted")
	}
	if _, err = Append(root, s.ID, EventRunStarted, RunStarted{RunID: "r1", WorkKind: "session", Intent: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err = Append(root, s.ID, EventRunEvent, RunEvent{ID: "wrong-session", RunID: "r1", SessionID: "other", RunSeq: 1, At: time.Now().UTC(), Kind: "text_delta"}); err == nil {
		t.Fatal("cross-session event accepted")
	}
}
