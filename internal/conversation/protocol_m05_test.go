package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// rewindFixture builds a project with a session, a snapshot-owning ready
// candidate, and the stores the rewind protocol needs.
type rewindFixture struct {
	root          string
	session       sessionlog.SessionInfo
	snapshots     *candidate.SnapshotStore
	store         *store.Store
	candidateRoot string
	snap          candidate.FileSnapshot
	currentDigest string
}

func newRewindFixture(t *testing.T) rewindFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "design.txt"), []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := candidate.NewSnapshotStore(root, 1<<20, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "cand-1")
	created, err := candidate.CreateCandidate("cand-1", root, filepath.Dir(candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshots.Create(session.ID, "cand-1", "run-1", "pre:write_file", created.CandidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.CandidateRoot, "design.txt"), []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	_, currentDigest, err := candidate.BuildManifest(created.CandidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveCandidate(context.Background(), store.CandidateRecord{
		Candidate: candidate.Candidate{ID: "cand-1", FormalRoot: created.FormalRoot, CandidateRoot: created.CandidateRoot, BaselineDigest: created.BaselineDigest, CandidateDigest: currentDigest, Status: "ready"},
		ActionID:  "tool-run-run-1",
		GoalID:    "session-" + session.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventSnapshot, sessionlog.SnapshotRef{
		SnapshotID: snap.SnapshotID, SessionID: session.ID, CandidateID: "cand-1", RunID: "run-1",
		Label: "pre:write_file", Digest: snap.Digest, CreatedAt: snap.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	return rewindFixture{root: root, session: session, snapshots: snapshots, store: db, candidateRoot: created.CandidateRoot, snap: snap, currentDigest: currentDigest}
}

func (f rewindFixture) service() *Service {
	return &Service{deps: Deps{Store: f.store, ProjectRoot: f.root, Snapshots: f.snapshots}}
}

func (f rewindFixture) rewindRequest() ClientMsg {
	return ClientMsg{Op: "snapshot_rewind", SessionID: f.session.ID, CandidateID: "cand-1", SnapshotID: f.snap.SnapshotID, CandidateDigest: f.currentDigest}
}

// A confirmed rewind restores the snapshot content, finalizes the journal,
// updates the stored candidate digest, and records pending then completed
// events in order.
func TestRewindSnapshotRestoresCandidate(t *testing.T) {
	f := newRewindFixture(t)
	record, err := f.service().rewindSnapshot(context.Background(), f.rewindRequest())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != sessionlog.RewindCompleted {
		t.Fatalf("rewind record = %+v", record)
	}
	content, err := os.ReadFile(filepath.Join(f.candidateRoot, "design.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Fatalf("candidate content = %q, want v1", content)
	}
	if _, digest, err := candidate.BuildManifest(f.candidateRoot); err != nil || digest != f.snap.Digest {
		t.Fatalf("candidate digest = %s, want snapshot %s (%v)", digest, f.snap.Digest, err)
	}
	loaded, err := f.store.GetCandidate(context.Background(), "cand-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Candidate.Status != "ready" || loaded.Candidate.CandidateDigest != f.snap.Digest {
		t.Fatalf("stored candidate = %+v", loaded.Candidate)
	}
	if _, ok, err := f.store.UnfinishedRewindFor(context.Background(), "cand-1"); err != nil || ok {
		t.Fatalf("rewind journal not finalized: ok=%t err=%v", ok, err)
	}
	transcript, err := sessionlog.Replay(f.root, f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	lastSnapshotSeq := uint64(0)
	for _, e := range transcript.Events {
		switch e.Type {
		case sessionlog.EventSnapshot:
			lastSnapshotSeq = e.Seq
		case sessionlog.EventRewind:
			var r sessionlog.RewindRecord
			raw, _ := json.Marshal(e.Data)
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			statuses = append(statuses, r.Status)
			if e.Seq < lastSnapshotSeq {
				t.Fatal("rewind event recorded before its snapshot")
			}
		}
	}
	if len(statuses) != 2 || statuses[0] != sessionlog.RewindPending || statuses[1] != sessionlog.RewindCompleted {
		t.Fatalf("rewind events = %v", statuses)
	}
	// The formal project is untouched.
	formal, err := os.ReadFile(filepath.Join(f.root, "design.txt"))
	if err != nil || string(formal) != "v1" {
		t.Fatalf("formal project changed: %q %v", formal, err)
	}
}

// Every ownership, lifecycle, and freshness refusal is explicit.
func TestRewindSnapshotRefusals(t *testing.T) {
	f := newRewindFixture(t)
	ctx := context.Background()

	// Snapshot recorded by another session is not visible here.
	other, err := sessionlog.Create(f.root, "other")
	if err != nil {
		t.Fatal(err)
	}
	request := f.rewindRequest()
	request.SessionID = other.ID
	if _, err := f.service().rewindSnapshot(ctx, request); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("cross-session rewind = %v", err)
	}

	// A moved candidate digest must be re-confirmed.
	request = f.rewindRequest()
	request.CandidateDigest = "stale"
	if _, err := f.service().rewindSnapshot(ctx, request); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale digest rewind = %v", err)
	}

	// An active run in the session blocks the rewind.
	svc := f.service()
	svc.activeRuns = map[string]string{"run-busy": f.session.ID}
	if _, err := svc.rewindSnapshot(ctx, f.rewindRequest()); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("active-run rewind = %v", err)
	}

	// Accepted candidates can never be rewound.
	accepted := newRewindFixture(t)
	if _, err := accepted.store.DB().ExecContext(ctx, "UPDATE candidates SET status='accepted' WHERE id='cand-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := accepted.service().rewindSnapshot(ctx, accepted.rewindRequest()); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("accepted-candidate rewind = %v", err)
	}

	// An unfinished rewind journal blocks a second attempt.
	journaled := newRewindFixture(t)
	if err := journaled.store.BeginRewind(ctx, store.RewindJournal{ID: "j-1", CandidateID: "cand-1", SnapshotID: journaled.snap.SnapshotID, ExpectedDigest: journaled.currentDigest, TargetDigest: journaled.snap.Digest, StagingDir: filepath.Join(t.TempDir(), "staging")}); err != nil {
		t.Fatal(err)
	}
	if _, err := journaled.service().rewindSnapshot(ctx, journaled.rewindRequest()); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("double rewind = %v", err)
	}
}

// snapshot_list exposes exactly the snapshots the requesting session
// recorded for the candidate.
func TestListSnapshotsOwnership(t *testing.T) {
	f := newRewindFixture(t)
	refs, err := f.service().listSnapshots(ClientMsg{SessionID: f.session.ID, CandidateID: "cand-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].SnapshotID != f.snap.SnapshotID || refs[0].Label != "pre:write_file" {
		t.Fatalf("snapshots = %+v", refs)
	}
	other, err := sessionlog.Create(f.root, "other")
	if err != nil {
		t.Fatal(err)
	}
	refs, err = f.service().listSnapshots(ClientMsg{SessionID: other.ID, CandidateID: "cand-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("other session sees snapshots: %+v", refs)
	}
	if _, err = f.service().listSnapshots(ClientMsg{SessionID: "missing", CandidateID: "cand-1"}); err == nil {
		t.Fatal("unknown session listed snapshots")
	}
}

// Replies answer a pending question exactly once; unknown, answered, and
// cross-session questions are explicit refusals, and the state survives a
// service restart because it is replayed from the log.
func TestQuestionReplyLifecycle(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	question := sessionlog.PendingQuestion{
		QuestionID: "q-1", WorkRef: "session/" + session.ID, SessionID: session.ID, RunID: "run-1",
		Prompt: "继续吗？", CreatedAt: time.Now().UTC(), Status: sessionlog.QuestionPending,
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventQuestion, question); err != nil {
		t.Fatal(err)
	}
	newService := func() *Service { return &Service{deps: Deps{ProjectRoot: root}} }

	questions, err := newService().listQuestions(ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].Status != sessionlog.QuestionPending {
		t.Fatalf("questions = %+v", questions)
	}

	if _, err := newService().replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-missing", Text: "x"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown question reply = %v", err)
	}

	msgs, err := newService().replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-1", Text: "继续"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Reply == nil || msgs[0].Reply.QuestionID != "q-1" || msgs[0].Reply.ReplyText != "继续" {
		t.Fatalf("reply msgs = %+v", msgs)
	}

	// A restart (fresh service over the same log) sees the answered state.
	questions, err = newService().listQuestions(ClientMsg{SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].Status != sessionlog.QuestionReplied {
		t.Fatalf("questions after reply = %+v", questions)
	}
	if _, err := newService().replyQuestion(context.Background(), ClientMsg{SessionID: session.ID, QuestionID: "q-1", Text: "again"}); err == nil || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("duplicate reply = %v", err)
	}
	// The question is invisible to another session.
	other, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newService().replyQuestion(context.Background(), ClientMsg{SessionID: other.ID, QuestionID: "q-1", Text: "x"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("cross-session reply = %v", err)
	}
}

// session_search finds text across sessions and reports corrupt logs
// instead of silently skipping them.
func TestSearchSessionsOp(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Kind: "text", Text: "修一下原理图封装"}); err != nil {
		t.Fatal(err)
	}
	other, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, other.ID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Kind: "text", Text: "无关内容"}); err != nil {
		t.Fatal(err)
	}
	corrupt, err := sessionlog.Create(root, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	path, err := sessionlog.SessionPath(root, corrupt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json\n"), 0600); err != nil {
		t.Fatal(err)
	}

	svc := &Service{deps: Deps{ProjectRoot: root}}
	result, err := svc.searchSessions(ClientMsg{ProjectRoot: root, Text: "原理图"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Session.ID != session.ID {
		t.Fatalf("hits = %+v", result.Hits)
	}
	if len(result.Corrupt) != 1 || result.Corrupt[0].SessionID != corrupt.ID {
		t.Fatalf("corrupt = %+v", result.Corrupt)
	}
}
