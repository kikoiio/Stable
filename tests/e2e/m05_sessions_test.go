package e2e

// M05 end-to-end evidence: multi-session search and restart-consistent
// restore, over-threshold compaction with an audit-safe persistent boundary,
// candidate snapshots with rewind and crash reconciliation, and /say //reply
// semantics that never move goal facts. These tests drive the real
// conversation service over its unix socket; no sandbox is required.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/prompt"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type m05Chat struct{ calls int }

func (p *m05Chat) GenerateChat(_ context.Context, messages []decision.ChatMessage) (string, error) {
	p.calls++
	for _, m := range messages {
		// Compaction turns carry the summarization instruction; answer with a
		// bounded summary, otherwise with a short chat reply.
		if strings.Contains(m.Content, "Summarize faithfully") {
			return "摘要：较早的会话内容。", nil
		}
	}
	return "好的，已记录。", nil
}

type m05Server struct {
	svc  *conversation.Service
	conn net.Conn
	ctx  context.Context
	stop context.CancelFunc
}

func m05Serve(t *testing.T, deps conversation.Deps, socket string) *m05Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	deps.SocketPath = socket
	svc, err := conversation.Serve(ctx, deps)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var conn net.Conn
	for i := 0; i < 100; i++ {
		conn, err = net.Dial("unix", socket)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("dial chat socket: %v", err)
	}
	server := &m05Server{svc: svc, conn: conn, ctx: ctx, stop: cancel}
	t.Cleanup(func() {
		conn.Close()
		svc.Close()
		cancel()
	})
	return server
}

// m05Call sends one op and collects every server message through done.
func m05Call(t *testing.T, server *m05Server, msg conversation.ClientMsg) []conversation.ServerMsg {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.conn.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(server.conn)
	var out []conversation.ServerMsg
	for {
		var m conversation.ServerMsg
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode server message: %v", err)
		}
		out = append(out, m)
		if m.Type == "done" {
			return out
		}
	}
}

func m05Error(msgs []conversation.ServerMsg) string {
	for _, m := range msgs {
		if m.Type == "error" {
			return m.Error
		}
	}
	return ""
}

func m05SessionID(t *testing.T, msgs []conversation.ServerMsg) string {
	t.Helper()
	for _, m := range msgs {
		if m.Type == "session" && m.Session != nil {
			return m.Session.ID
		}
	}
	t.Fatalf("no session in %+v", msgs)
	return ""
}

func m05AppendChat(t *testing.T, root, sessionID, role, text string) {
	t.Helper()
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventMessage, sessionlog.Message{Role: role, Kind: "text", Text: text}); err != nil {
		t.Fatal(err)
	}
}

// Step 1: create several sessions, search them by content, restart the
// service, and restore transcript and agent context identically.
func TestM05SessionSearchAndRestartRestore(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sessions := map[string]string{}
	for name, keyword := range map[string]string{"alpha": "原理图封装", "beta": "走线宽度"} {
		info, err := sessionlog.Create(root, name)
		if err != nil {
			t.Fatal(err)
		}
		sessions[name] = info.ID
		m05AppendChat(t, root, info.ID, "user", "帮我检查"+keyword+"的问题")
		m05AppendChat(t, root, info.ID, "assistant", "已分析"+keyword)
	}
	corrupt, err := sessionlog.Create(root, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	corruptPath, err := sessionlog.SessionPath(root, corrupt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{broken\n"), 0600); err != nil {
		t.Fatal(err)
	}

	socket := filepath.Join(root, "chat.sock")
	deps := conversation.Deps{Store: db, ProjectRoot: root, ChatProvider: &m05Chat{}}
	server := m05Serve(t, deps, socket)

	found := m05Call(t, server, conversation.ClientMsg{Op: "session_search", ProjectRoot: root, Text: "走线宽度"})
	var result *sessionlog.SearchResult
	for _, m := range found {
		if m.Type == "search" {
			result = m.Search
		}
	}
	if result == nil {
		t.Fatalf("no search response in %+v", found)
	}
	if len(result.Hits) != 2 {
		t.Fatalf("hits = %+v", result.Hits)
	}
	for _, hit := range result.Hits {
		if hit.Session.ID != sessions["beta"] {
			t.Fatalf("hit from wrong session: %+v", hit)
		}
	}
	if len(result.Corrupt) != 1 || result.Corrupt[0].SessionID != corrupt.ID {
		t.Fatalf("corrupt log not reported: %+v", result.Corrupt)
	}

	load := func(s *m05Server, id string) sessionlog.Transcript {
		t.Helper()
		msgs := m05Call(t, s, conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: id})
		for _, m := range msgs {
			if m.Type == "transcript" && m.Transcript != nil {
				return *m.Transcript
			}
		}
		t.Fatalf("no transcript in %+v", msgs)
		return sessionlog.Transcript{}
	}
	before := load(server, sessions["alpha"])
	server.conn.Close()
	server.svc.Close()
	server.stop()

	// Restart: a fresh service over the same project must replay the exact
	// transcript and derive the exact agent context. Load activity events are
	// appended by each load itself and are excluded from the comparison.
	restarted := m05Serve(t, deps, socket)
	after := load(restarted, sessions["alpha"])
	contentEvents := func(events []sessionlog.Event) []sessionlog.Event {
		out := []sessionlog.Event{}
		for _, e := range events {
			if e.Type != sessionlog.EventActivity {
				out = append(out, e)
			}
		}
		return out
	}
	if !reflect.DeepEqual(contentEvents(before.Events), contentEvents(after.Events)) {
		t.Fatal("transcript changed across restart")
	}
	contextBefore := prompt.MessagesFromItems(sessionlog.Project(before).Items)
	contextAfter := prompt.MessagesFromItems(sessionlog.Project(after).Items)
	if !reflect.DeepEqual(contextBefore, contextAfter) {
		t.Fatal("agent context changed across restart")
	}
	if len(contextAfter) != 2 {
		t.Fatalf("context = %+v", contextAfter)
	}
	if msgs := m05Call(t, restarted, conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: corrupt.ID}); m05Error(msgs) == "" {
		t.Fatal("corrupt session loaded without an explicit error")
	}
}

// Step 2: crossing the compaction threshold persists a boundary while every
// original event stays in the log for audit, and later turns still work.
func TestM05CompactionBoundaryAudit(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	info, err := sessionlog.Create(root, "compact")
	if err != nil {
		t.Fatal(err)
	}
	server := m05Serve(t, conversation.Deps{Store: db, ProjectRoot: root, ChatProvider: &m05Chat{}, ContextWindowTokens: 4096}, filepath.Join(root, "chat.sock"))

	longText := strings.Repeat("请详细分析这段约束。", 60)
	turns := 0
	boundarySeen := false
	for ; turns < 30 && !boundarySeen; turns++ {
		msgs := m05Call(t, server, conversation.ClientMsg{Op: "chat", ProjectRoot: root, SessionID: info.ID, Text: longText})
		if errText := m05Error(msgs); errText != "" {
			t.Fatalf("chat turn %d failed: %s", turns, errText)
		}
		transcript, err := sessionlog.Replay(root, info.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range transcript.Events {
			if e.Type == sessionlog.EventBoundary {
				boundarySeen = true
			}
		}
	}
	if !boundarySeen {
		t.Fatal("no compaction boundary after crossing the threshold")
	}
	transcript, err := sessionlog.Replay(root, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	var boundary sessionlog.Boundary
	for _, e := range transcript.Events {
		switch e.Type {
		case sessionlog.EventMessage:
			messages++
		case sessionlog.EventBoundary:
			raw, _ := json.Marshal(e.Data)
			if err := json.Unmarshal(raw, &boundary); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Append-only audit: every original user/assistant message is still in
	// the log; the boundary replaces them only in the projection.
	if messages != 2*turns {
		t.Fatalf("audited messages = %d, want %d", messages, 2*turns)
	}
	if boundary.EffectiveScope() != sessionlog.BoundaryScopeSession || boundary.FromSeq == 0 || boundary.ToSeq <= boundary.FromSeq || boundary.Summary == "" {
		t.Fatalf("boundary = %+v", boundary)
	}
	items := sessionlog.Project(transcript).Items
	hasSummary := false
	for _, item := range items {
		if item.Kind == sessionlog.ItemSummary {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Fatal("projection lost the compaction summary")
	}
	// The session keeps working after compaction.
	if msgs := m05Call(t, server, conversation.ClientMsg{Op: "chat", ProjectRoot: root, SessionID: info.ID, Text: "继续"}); m05Error(msgs) != "" {
		t.Fatalf("post-compaction turn failed: %s", m05Error(msgs))
	}
}

// m05RewindFixture builds a project with a ready candidate, two recorded
// snapshots, and a session that owns them.
type m05RewindFixture struct {
	root          string
	sessionID     string
	candidateRoot string
	snapV1        candidate.FileSnapshot
	snapV2        candidate.FileSnapshot
	currentDigest string
	formalDigest  string
}

func m05NewRewindFixture(t *testing.T, db *store.Store, dir string) m05RewindFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "board.kicad_pcb"), []byte("layout-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	_, formalDigest, err := candidate.BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "work")
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := candidate.NewSnapshotStore(root, 1<<22, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := candidate.CreateCandidate("cand-1", root, dir)
	if err != nil {
		t.Fatal(err)
	}
	snapV1, err := snapshots.Create(session.ID, "cand-1", "run-1", "snap-v1", created.CandidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.CandidateRoot, "board.kicad_pcb"), []byte("layout-v2"), 0600); err != nil {
		t.Fatal(err)
	}
	snapV2, err := snapshots.Create(session.ID, "cand-1", "run-1", "snap-v2", created.CandidateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.CandidateRoot, "notes.txt"), []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	_, currentDigest, err := candidate.BuildManifest(created.CandidateRoot)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, snap := range []candidate.FileSnapshot{snapV1, snapV2} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventSnapshot, sessionlog.SnapshotRef{
			SnapshotID: snap.SnapshotID, SessionID: session.ID, CandidateID: "cand-1", RunID: "run-1",
			Label: snap.Label, Digest: snap.Digest, CreatedAt: snap.CreatedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return m05RewindFixture{root: root, sessionID: session.ID, candidateRoot: created.CandidateRoot, snapV1: snapV1, snapV2: snapV2, currentDigest: currentDigest, formalDigest: formalDigest}
}

// Step 3: multiple candidate changes produce per-session snapshots; a rewind
// restores the chosen one, a restart reconciles an interrupted journal, and
// the formal project never changes.
func TestM05SnapshotRewindAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := m05NewRewindFixture(t, db, filepath.Join(dir, "candidates"))
	snapshots, err := candidate.NewSnapshotStore(f.root, 1<<22, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := m05Serve(t, conversation.Deps{Store: db, ProjectRoot: f.root, Snapshots: snapshots, ChatProvider: &m05Chat{}}, filepath.Join(dir, "chat.sock"))

	listed := m05Call(t, server, conversation.ClientMsg{Op: "snapshot_list", SessionID: f.sessionID, CandidateID: "cand-1"})
	var refs []sessionlog.SnapshotRef
	for _, m := range listed {
		if m.Type == "snapshots" {
			refs = m.Snapshots
		}
	}
	if len(refs) != 2 || refs[0].Label != "snap-v1" || refs[1].Label != "snap-v2" {
		t.Fatalf("snapshot list = %+v", refs)
	}

	// A stale expected digest is refused before anything moves.
	stale := m05Call(t, server, conversation.ClientMsg{Op: "snapshot_rewind", SessionID: f.sessionID, CandidateID: "cand-1", SnapshotID: f.snapV1.SnapshotID, CandidateDigest: "stale"})
	if errText := m05Error(stale); !strings.Contains(errText, "changed since") {
		t.Fatalf("stale digest rewind = %q", errText)
	}
	if _, digest, err := candidate.BuildManifest(f.candidateRoot); err != nil || digest != f.currentDigest {
		t.Fatal("refused rewind still moved the candidate")
	}

	rewound := m05Call(t, server, conversation.ClientMsg{Op: "snapshot_rewind", SessionID: f.sessionID, CandidateID: "cand-1", SnapshotID: f.snapV1.SnapshotID, CandidateDigest: f.currentDigest})
	if errText := m05Error(rewound); errText != "" {
		t.Fatalf("rewind failed: %s", errText)
	}
	var record *sessionlog.RewindRecord
	for _, m := range rewound {
		if m.Type == "rewind" {
			record = m.Rewind
		}
	}
	if record == nil || record.Status != sessionlog.RewindCompleted {
		t.Fatalf("rewind response = %+v", rewound)
	}
	content, err := os.ReadFile(filepath.Join(f.candidateRoot, "board.kicad_pcb"))
	if err != nil || string(content) != "layout-v1" {
		t.Fatalf("candidate content = %q %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(f.candidateRoot, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("file created after the snapshot survived the rewind")
	}
	if _, digest, err := candidate.BuildManifest(f.candidateRoot); err != nil || digest != f.snapV1.Digest {
		t.Fatal("candidate digest does not match the restored snapshot")
	}
	if _, formalAfter, err := candidate.BuildManifest(f.root); err != nil || formalAfter != f.formalDigest {
		t.Fatal("formal project changed during rewind")
	}

	// Crash window: a journal left prepared with its staging directory on
	// disk is reconciled at the next startup — nothing moved, so the journal
	// finalizes and the staging directory is cleaned.
	db2, err := store.Open(filepath.Join(dir, "state2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	f2 := m05NewRewindFixture(t, db2, filepath.Join(dir, "candidates2"))
	staging := filepath.Join(filepath.Dir(f2.candidateRoot), ".rewind-staging-crash")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := db2.BeginRewind(ctx, store.RewindJournal{ID: "crash-1", CandidateID: "cand-1", SnapshotID: f2.snapV1.SnapshotID, ExpectedDigest: f2.currentDigest, TargetDigest: f2.snapV1.Digest, StagingDir: staging}); err != nil {
		t.Fatal(err)
	}
	db2.Close()
	reopened, err := store.Open(filepath.Join(dir, "state2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ReconcileRewinds(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := reopened.UnfinishedRewindFor(ctx, "cand-1"); err != nil || ok {
		t.Fatalf("journal unfinished after reconcile: ok=%t err=%v", ok, err)
	}
	if _, err := os.Lstat(staging); !os.IsNotExist(err) {
		t.Fatal("staging directory survived reconciliation")
	}
	if _, digest, err := candidate.BuildManifest(f2.candidateRoot); err != nil || digest != f2.currentDigest {
		t.Fatal("reconciliation moved a candidate whose swap never happened")
	}
}

// Step 4: /say persists ordered instructions the next decision round consumes
// in order; /reply answers exactly one pending question; neither compaction,
// rewind, nor these messages ever change goal evidence or the verified
// conclusion.
func TestM05SayReplyAndGoalFactBoundary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO goals(id,objective,criteria_json,allowed_root,allowed_capabilities_json,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		"goal-m05", "验证目标事实边界", `[]`, root, `[]`, string(core.GoalVerified), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO agents(id,goal_id,status) VALUES(?,?,?)`, "agent-m05", "goal-m05", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO computer_sessions(id,goal_id,status) VALUES(?,?,?)`, "cs-m05", "goal-m05", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO evidence(id,goal_id,criterion_id,artifact_id,kind,result,report_path,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"ev-1", "goal-m05", "c-1", "art-1", "manual", "pass", "report.md", now); err != nil {
		t.Fatal(err)
	}
	server := m05Serve(t, conversation.Deps{Store: db, ProjectRoot: root, ChatProvider: &m05Chat{}}, filepath.Join(root, "chat.sock"))

	for _, text := range []string{"指令一", "指令二"} {
		msgs := m05Call(t, server, conversation.ClientMsg{Op: "say", Goal: "goal-m05", Text: text})
		if errText := m05Error(msgs); errText != "" {
			t.Fatalf("say %q failed: %s", text, errText)
		}
	}
	pending, err := db.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("queued say events = %+v", pending)
	}
	// The next decision round consumes queued instructions in queue order.
	for i, want := range []string{"指令一", "指令二"} {
		if pending[i].Kind != core.EventKindUserMessage {
			t.Fatalf("event %d kind = %q", i, pending[i].Kind)
		}
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(pending[i].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		msg, err := messageText(ctx, db, payload.MessageID)
		if err != nil || msg != want {
			t.Fatalf("consumption order %d = %q, want %q (%v)", i, msg, want, err)
		}
		// Consumption follows the real lifecycle: signaled, then processed.
		if err := db.SetEventStatus(ctx, pending[i].ID, "signaled"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetEventStatus(ctx, pending[i].ID, "processed"); err != nil {
			t.Fatal(err)
		}
	}

	// /reply answers only an explicit pending question, exactly once.
	session, err := sessionlog.Create(root, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-1", WorkKind: "session", Intent: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, session.ID, sessionlog.EventQuestion, sessionlog.PendingQuestion{
		QuestionID: "q-1", WorkRef: "session/" + session.ID, SessionID: session.ID, RunID: "run-1",
		Prompt: "是否继续？", CreatedAt: time.Now().UTC(), Status: sessionlog.QuestionPending,
	}); err != nil {
		t.Fatal(err)
	}
	if msgs := m05Call(t, server, conversation.ClientMsg{Op: "reply", SessionID: session.ID, QuestionID: "q-missing", Text: "x"}); !strings.Contains(m05Error(msgs), "does not exist") {
		t.Fatalf("unknown question reply = %+v", msgs)
	}
	if msgs := m05Call(t, server, conversation.ClientMsg{Op: "reply", SessionID: session.ID, QuestionID: "q-1", Text: "继续"}); m05Error(msgs) != "" {
		t.Fatalf("reply failed: %s", m05Error(msgs))
	}
	if msgs := m05Call(t, server, conversation.ClientMsg{Op: "reply", SessionID: session.ID, QuestionID: "q-1", Text: "重复"}); !strings.Contains(m05Error(msgs), "already answered") {
		t.Fatalf("duplicate reply = %+v", msgs)
	}
	questions := m05Call(t, server, conversation.ClientMsg{Op: "question_list", SessionID: session.ID})
	for _, m := range questions {
		if m.Type != "questions" {
			continue
		}
		if len(m.Questions) != 1 || m.Questions[0].Status != sessionlog.QuestionReplied {
			t.Fatalf("questions = %+v", m.Questions)
		}
	}

	// Goal facts are untouched by every event above.
	snapshot, err := db.GetGoalSnapshot(ctx, "goal-m05")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalVerified {
		t.Fatalf("goal status moved to %q", snapshot.Goal.Status)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].Result != "pass" || snapshot.Evidence[0].InvalidatedReason != "" {
		t.Fatalf("evidence changed: %+v", snapshot.Evidence)
	}
}

func messageText(ctx context.Context, db *store.Store, id string) (string, error) {
	var text string
	err := db.DB().QueryRowContext(ctx, `SELECT text FROM session_messages WHERE id=?`, id).Scan(&text)
	return text, err
}
