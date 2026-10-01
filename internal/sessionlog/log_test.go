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
