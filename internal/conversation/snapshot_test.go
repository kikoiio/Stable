package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// consumeRun turns snapshot metadata riding a tool result into session
// snapshot events in stream order, keeping conversation the only log writer.
func TestConsumeRunPersistsSnapshotEvents(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	outcomePayload, _ := json.Marshal(agent.ToolOutcome{
		CallID: "call-1", ToolName: "write_file", Status: agent.ToolSucceeded, Content: "wrote",
		Snapshots: []agent.SnapshotMeta{
			{SnapshotID: "snap-pre", CandidateID: "cand-1", RunID: "run-1", Label: "pre:write_file", Digest: "d-pre", CreatedAt: created},
			{SnapshotID: "snap-post", CandidateID: "cand-1", RunID: "run-1", Label: "post:write_file", Digest: "d-post", CreatedAt: created},
		},
	})
	events := make(chan agent.ExecutionEvent, 2)
	done := make(chan agent.RunOutcome, 1)
	events <- agent.ExecutionEvent{ID: "evt-1", RunID: "run-1", SessionID: session.ID, RunSeq: 1, At: created, Kind: agent.EventToolExecResult, Payload: outcomePayload}
	events <- agent.ExecutionEvent{ID: "evt-2", RunID: "run-1", SessionID: session.ID, RunSeq: 2, At: created.Add(time.Millisecond), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`)}
	close(events)
	done <- agent.RunOutcome{RunID: "run-1", Status: agent.RunCompleted}
	close(done)
	runner := &fixedRunner{handle: &agent.RunHandle{Events: events, Done: done}}
	updates := make(chan ServerMsg, 16)
	svc := &Service{deps: Deps{Runner: runner, ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{updates: {ch: updates}}}
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "write", Model: "mock"}
	if err := svc.startRun(context.Background(), ClientMsg{SessionID: session.ID, Run: &request}, updates); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-updates:
			if msg.Type == "run_outcome" {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for run outcome")
		}
	}
done:
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snaps []sessionlog.SnapshotRef
	for i, event := range transcript.Events {
		if event.Type != sessionlog.EventSnapshot {
			continue
		}
		var ref sessionlog.SnapshotRef
		raw, _ := json.Marshal(event.Data)
		if err := json.Unmarshal(raw, &ref); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, ref)
		// Snapshot events land immediately after their tool result event.
		if prev := transcript.Events[i-1].Type; prev != sessionlog.EventRunEvent && prev != sessionlog.EventSnapshot {
			t.Fatalf("snapshot event not in stream order at seq %d", event.Seq)
		}
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshot events = %d, want 2", len(snaps))
	}
	if snaps[0].SnapshotID != "snap-pre" || snaps[0].SessionID != session.ID || snaps[0].CandidateID != "cand-1" || snaps[0].RunID != "run-1" || snaps[0].Digest != "d-pre" {
		t.Fatalf("pre snapshot event = %+v", snaps[0])
	}
	if snaps[1].SnapshotID != "snap-post" || snaps[1].Label != "post:write_file" {
		t.Fatalf("post snapshot event = %+v", snaps[1])
	}
}

// A candidate blocked by a failed checkpoint stays blocked at run finalize:
// it is neither frozen ready nor cleaned up, so it can never be accepted.
func TestFinalizeRunCandidateKeepsBlockedCandidate(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidateRoot := filepath.Join(root, "candidates", "cand-1")
	if err := os.MkdirAll(candidateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "changed.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record := store.CandidateRecord{
		Candidate: candidate.Candidate{ID: "cand-1", FormalRoot: formal, CandidateRoot: candidateRoot, Status: "blocked"},
		ActionID:  "tool-run-run-1",
		GoalID:    "session-sess-1",
	}
	if err := db.SaveCandidate(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{RunID: "run-1", SessionID: "sess-1", AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot}
	bounds, _ := json.Marshal(authority)
	svc := &Service{deps: Deps{Store: db}}
	request := agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: "sess-1"}, PermissionBounds: bounds}
	if err := svc.finalizeRunCandidate(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.GetCandidate(context.Background(), "cand-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Candidate.Status != "blocked" {
		t.Fatalf("blocked candidate became %q", loaded.Candidate.Status)
	}
	if _, err := os.Lstat(filepath.Join(candidateRoot, "changed.txt")); err != nil {
		t.Fatal("blocked candidate content was cleaned up")
	}
}
