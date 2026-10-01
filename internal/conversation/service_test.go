package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/appconfig"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/dependency"
	"stable/internal/store"
)

type dependencyFixtureCollector struct{ snapshots []core.DependencySnapshot }

func (c dependencyFixtureCollector) Collect(context.Context, core.Goal) ([]core.DependencySnapshot, error) {
	return c.snapshots, nil
}

type perGoalDependencyCollector map[string][]core.DependencySnapshot

func (c perGoalDependencyCollector) Collect(_ context.Context, goal core.Goal) ([]core.DependencySnapshot, error) {
	return c[goal.ID], nil
}

func conversationDependencySet(fingerprint string) []core.DependencySnapshot {
	return []core.DependencySnapshot{
		{SchemaVersion: 1, Family: core.CheckFamilyERC, Sources: []core.DependencySource{{Kind: "project", Identity: "project", Digest: fingerprint, State: "available"}}, CheckerID: "kicad-cli-erc", CheckerVersion: "9", Fingerprint: fingerprint, Available: true},
		{SchemaVersion: 1, Family: core.CheckFamilyConnection, Sources: []core.DependencySource{{Kind: "checker", Identity: "stable", Digest: "1", State: "available"}}, CheckerID: "sensor-connection-check", CheckerVersion: "1", Fingerprint: "connection", Available: true},
	}
}

func TestStatusRefreshesDependenciesBeforeListingGoals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateGoal(ctx, core.Goal{ID: "g", ArtifactPath: filepath.Join(dir, "sensor.kicad_sch"), AllowedRoot: dir, CurrentArtifactID: "design", CriteriaRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileDependencies(ctx, "g", conversationDependencySet("before")); err != nil {
		t.Fatal(err)
	}
	refresher := &dependency.Refresher{State: s, Collector: dependencyFixtureCollector{conversationDependencySet("after")}}
	svc := &Service{deps: Deps{Store: s, Refresher: refresher}, statuses: map[string]core.GoalStatus{}}
	messages, err := svc.handle(ctx, ClientMsg{Op: "status"})
	if err != nil || len(messages) != 1 || messages[0].Goal == nil || messages[0].Goal.Status != core.GoalPendingReverification {
		t.Fatalf("status response did not include refreshed state: %+v err=%v", messages, err)
	}
	snapshot, err := s.GetGoalSnapshot(ctx, "g")
	if err != nil || snapshot.Goal.Status != core.GoalPendingReverification || snapshot.Goal.DependencyRevision != 1 {
		t.Fatalf("dependency change not persisted before response: %+v err=%v", snapshot.Goal, err)
	}
}

func TestStatusRefreshesOnlyChangedGoalInMultiGoalList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"changed", "stable"} {
		if _, err = s.CreateGoal(ctx, core.Goal{ID: id, ArtifactPath: filepath.Join(dir, id+".kicad_sch"), AllowedRoot: dir, CurrentArtifactID: "design", CriteriaRevision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	changedBefore := conversationDependencySet("before")
	stable := conversationDependencySet("stable")
	if _, err = s.ReconcileDependencies(ctx, "changed", changedBefore); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileDependencies(ctx, "stable", stable); err != nil {
		t.Fatal(err)
	}
	refresher := &dependency.Refresher{State: s, Collector: perGoalDependencyCollector{"changed": conversationDependencySet("after"), "stable": stable}}
	svc := &Service{deps: Deps{Store: s, Refresher: refresher}, statuses: map[string]core.GoalStatus{}}
	messages, err := svc.handle(ctx, ClientMsg{Op: "status"})
	if err != nil || len(messages) != 2 {
		t.Fatalf("multi-goal status: %+v err=%v", messages, err)
	}
	statuses := map[string]core.GoalStatus{}
	for _, msg := range messages {
		statuses[msg.Goal.ID] = msg.Goal.Status
	}
	if statuses["changed"] != core.GoalPendingReverification || statuses["stable"] != core.GoalActive {
		t.Fatalf("dependency change leaked across goals: %v", statuses)
	}
	events, err := s.UnprocessedEvents(ctx)
	if err != nil || len(events) != 1 || events[0].GoalID != "changed" {
		t.Fatalf("goal-local wake event: %+v err=%v", events, err)
	}
}

type mockTranspiler struct {
	server *httptest.Server
}

func newMockTranspiler(t *testing.T, respond func(nl string) string) decision.StructuredProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		nl := ""
		if len(body.Messages) > 1 {
			nl = body.Messages[1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		content, _ := json.Marshal(respond(nl))
		_, _ = w.Write([]byte(`{"id":"resp-1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + string(content) + `}}]}`))
	}))
	t.Cleanup(server.Close)
	p, err := decision.NewProvider(newTestConfig(server.URL).Model)
	if err != nil {
		t.Fatal(err)
	}
	return p.(decision.StructuredProvider)
}

func newTestConfig(baseURL string) appconfig.AppConfig {
	return appconfig.AppConfig{Model: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: baseURL, APIKey: "test-key"}}
}

func startService(t *testing.T, provider decision.StructuredProvider) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.CreateGoal(context.Background(), core.Goal{ID: "goal-1", Objective: "repair sensor",
		AllowedRoot: dir, AllowedCapabilities: []string{"kicad.repair_connection"}}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "chat.sock")
	var chatProvider decision.ChatProvider
	if p, ok := provider.(decision.ChatProvider); ok {
		chatProvider = p
	}
	svc, err := Serve(context.Background(), Deps{
		Store: s, Provider: provider, ChatProvider: chatProvider, Temporal: "127.0.0.1:1", ProjectRoot: dir, RunRoot: filepath.Join(dir, "goals"),
		SocketPath: socket, PollEvery: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc, socket
}

func TestChatRepliesWithoutGoalOrWorkflowEvent(t *testing.T) {
	var requests [][]decision.ChatMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []decision.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body.Messages)
		content, _ := json.Marshal("你好，我可以帮你查看目标进度。")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + string(content) + `}}]}`))
	}))
	defer server.Close()
	model, err := decision.NewProvider(newTestConfig(server.URL).Model)
	if err != nil {
		t.Fatal(err)
	}
	svc, socket := startService(t, model.(decision.StructuredProvider))
	conn, reader := dial(t, socket)
	send(t, conn, ClientMsg{Op: "chat", Text: "你好"})
	user := readMsg(t, reader)
	agent := readMsg(t, reader)
	done := readMsg(t, reader)
	if user.Message == nil || user.Message.Role != core.MessageRoleUser || user.Message.GoalID != "" || agent.Message == nil || agent.Message.Role != core.MessageRoleAgent || !strings.Contains(agent.Message.Text, "你好") || done.Type != "done" {
		t.Fatalf("chat exchange: %+v %+v %+v", user, agent, done)
	}
	if len(requests) != 1 || len(requests[0]) < 2 || requests[0][len(requests[0])-1].Content != "你好" {
		t.Fatalf("model request: %+v", requests)
	}
	send(t, conn, ClientMsg{Op: "chat", Text: "还记得刚才说什么吗？"})
	readMsg(t, reader)
	readMsg(t, reader)
	if done := readMsg(t, reader); done.Type != "done" {
		t.Fatalf("second chat completion: %+v", done)
	}
	if len(requests) != 2 || len(requests[1]) != 4 || requests[1][1].Content != "你好" || requests[1][2].Content != agent.Message.Text {
		t.Fatalf("conversation context: %+v", requests)
	}
	events, err := svc.deps.Store.PendingEvents(context.Background())
	if err != nil || len(events) != 0 {
		t.Fatalf("chat queued goal events: %+v %v", events, err)
	}
	history, err := svc.deps.Store.ListMessages(context.Background())
	if err != nil || len(history) != 4 {
		t.Fatalf("chat history: %+v %v", history, err)
	}
}

func dial(t *testing.T, socket string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

func readMsg(t *testing.T, r *bufio.Reader) ServerMsg {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m ServerMsg
	if err = json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}

func send(t *testing.T, conn net.Conn, c ClientMsg) {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(c); err != nil {
		t.Fatal(err)
	}
}

// readConfirmExchange collects the messages a confirm of an existing goal
// produces — the criteria_confirm reply, the pending_reverification
// goal_update broadcast, and the one-shot done marker — in whichever order
// they arrive on the wire.
func readConfirmExchange(t *testing.T, r *bufio.Reader) (confirmMsg, update *ServerMsg, doneSeen bool) {
	t.Helper()
	for i := 0; i < 10 && (confirmMsg == nil || update == nil || !doneSeen); i++ {
		m := readMsg(t, r)
		switch {
		case m.Type == "message" && m.Message != nil && m.Message.Kind == core.MessageKindCriteriaConfirm:
			confirmMsg = &m
		case m.Type == "goal_update" && m.Goal != nil && m.Goal.Status == core.GoalPendingReverification:
			update = &m
		case m.Type == "done":
			doneSeen = true
		}
	}
	if confirmMsg == nil {
		t.Fatal("confirm reply missing")
	}
	if update == nil {
		t.Fatal("pending_reverification goal update missing")
	}
	return confirmMsg, update, doneSeen
}

// readDone skips any late broadcasts until the one-shot completion marker.
func readDone(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for i := 0; i < 10; i++ {
		m := readMsg(t, r)
		if m.Type == "done" {
			return
		}
	}
	t.Fatal("completion marker missing")
}

func TestLegacyHistoryIsNotReplayedButLiveGoalMessagesBroadcast(t *testing.T) {
	_, socket := startService(t, nil)
	a, _ := dial(t, socket)
	b, _ := dial(t, socket)
	ra, rb := bufio.NewReader(a), bufio.NewReader(b)

	// Live goal messages continue to broadcast to connected clients.
	send(t, a, ClientMsg{Op: "say", Goal: "goal-1", Text: "focus on J1"})
	msgA := readMsg(t, ra)
	if msgA.Type != "message" || msgA.Message.Text != "focus on J1" {
		t.Fatalf("client A: %+v", msgA)
	}
	msgB := readMsg(t, rb)
	if msgB.Type != "message" || msgB.Message.Text != "focus on J1" {
		t.Fatalf("client B missed broadcast: %+v", msgB)
	}

	// A new connection does not replay SQLite chat rows as a session transcript.
	a.Close()
	c, _ := dial(t, socket)
	rc := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	for {
		line, err := rc.ReadBytes('\n')
		if err != nil {
			break
		}
		var m ServerMsg
		if json.Unmarshal(line, &m) == nil && m.Type == "message" && m.Message != nil && m.Message.Text == "focus on J1" {
			t.Fatalf("legacy SQLite history leaked to new session client: %+v", m)
		}
	}
}

func TestSayQueuesEventWhenTemporalOffline(t *testing.T) {
	svc, socket := startService(t, nil)
	conn, _ := dial(t, socket)
	send(t, conn, ClientMsg{Op: "say", Goal: "goal-1", Text: "hello"})
	readMsg(t, bufio.NewReader(conn))
	events, err := svc.deps.Store.PendingEvents(context.Background())
	if err != nil || len(events) != 1 || events[0].Kind != core.EventKindUserMessage {
		t.Fatalf("pending events: %+v %v", events, err)
	}
	var payload struct {
		MessageID string `json:"message_id"`
	}
	if err = json.Unmarshal(events[0].Payload, &payload); err != nil || payload.MessageID == "" {
		t.Fatalf("payload: %s %v", events[0].Payload, err)
	}
}

func TestCreateGoalRejectUnverifiable(t *testing.T) {
	_, socket := startService(t, newMockTranspiler(t, func(nl string) string {
		return `{"status":"reject","criteria":[],"reason":"美观无法机器验证"}`
	}))
	conn, _ := dial(t, socket)
	r := bufio.NewReader(conn)
	send(t, conn, ClientMsg{Op: "create_goal", Text: "让它看起来美观"})
	msg := readMsg(t, r)
	if msg.Type != "message" || msg.Message == nil || msg.Message.Kind != core.MessageKindText {
		t.Fatalf("reject message: %+v", msg)
	}
}

func TestProposalAndConfirmUpdatesRunningGoal(t *testing.T) {
	svc, socket := startService(t, newMockTranspiler(t, func(nl string) string {
		return `{"status":"ok","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"reason":"ok"}`
	}))
	conn, _ := dial(t, socket)
	r := bufio.NewReader(conn)

	send(t, conn, ClientMsg{Op: "create_goal", Goal: "goal-1", Text: "ERC 全过"})
	proposalMsg := readMsg(t, r)
	if proposalMsg.Type != "message" || proposalMsg.Message.Kind != core.MessageKindCriteriaProposal {
		t.Fatalf("proposal message: %+v", proposalMsg)
	}
	proposal := readMsg(t, r)
	if proposal.Type != "proposal" || proposal.Proposal == nil || proposal.Proposal.Status != core.ProposalPending {
		t.Fatalf("proposal: %+v", proposal)
	}
	if done := readMsg(t, r); done.Type != "done" {
		t.Fatalf("missing completion marker: %+v", done)
	}

	// The proposal is session-level (goal existed but was not tied); confirm
	// with the running goal updates its criteria in place.
	send(t, conn, ClientMsg{Op: "confirm", ID: proposal.Proposal.ID, Goal: "goal-1"})
	// The confirm reply and the pending_reverification goal_update broadcast
	// race each other on the wire; accept them in either order.
	_, _, doneSeen := readConfirmExchange(t, r)
	if !doneSeen {
		readDone(t, r)
	}
	snap, err := svc.deps.Store.GetGoalSnapshot(context.Background(), "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.Status != core.GoalPendingReverification || snap.Goal.CriteriaRevision != 1 || len(snap.Goal.Criteria) != 1 || snap.Goal.Criteria[0].Kind != core.CriterionKindERCClean {
		t.Fatalf("criteria after confirm: %+v", snap.Goal)
	}
	events, _ := svc.deps.Store.PendingEvents(context.Background())
	if len(events) != 1 || events[0].Kind != core.EventKindCriteriaUpdate || events[0].ID != "criteria-confirm-"+proposal.Proposal.ID {
		t.Fatalf("criteria update event: %+v", events)
	}
	// Confirming twice must be rejected by the proposal state machine.
	send(t, conn, ClientMsg{Op: "confirm", ID: proposal.Proposal.ID, Goal: "goal-1"})
	errMsg := readMsg(t, r)
	if errMsg.Type != "error" {
		t.Fatalf("double confirm: %+v", errMsg)
	}
	// The rejected second confirm must not bump the revision again.
	snap, err = svc.deps.Store.GetGoalSnapshot(context.Background(), "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 1 {
		t.Fatalf("revision after double confirm: %+v", snap.Goal)
	}
}

func TestCreateGoalWithoutModelConfigured(t *testing.T) {
	_, socket := startService(t, nil)
	conn, _ := dial(t, socket)
	r := bufio.NewReader(conn)
	send(t, conn, ClientMsg{Op: "create_goal", Text: "ERC 全过"})
	msg := readMsg(t, r)
	if msg.Type != "error" {
		t.Fatalf("expected error, got %+v", msg)
	}
}

// Crash right after confirm: the confirmation reply went out while the wake
// failed (no Temporal in this test), so the persisted event must still be
// delivered after a worker restart, exactly once and with its original ID.
func TestConfirmInterruptThenRestartReplays(t *testing.T) {
	svc, socket := startService(t, newMockTranspiler(t, func(nl string) string {
		return `{"status":"ok","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"reason":"ok"}`
	}))
	conn, _ := dial(t, socket)
	r := bufio.NewReader(conn)

	send(t, conn, ClientMsg{Op: "create_goal", Goal: "goal-1", Text: "ERC 全过"})
	readMsg(t, r) // proposal message
	proposal := readMsg(t, r)
	if proposal.Type != "proposal" || proposal.Proposal == nil {
		t.Fatalf("proposal: %+v", proposal)
	}
	if done := readMsg(t, r); done.Type != "done" {
		t.Fatalf("create_goal completion: %+v", done)
	}
	send(t, conn, ClientMsg{Op: "confirm", ID: proposal.Proposal.ID, Goal: "goal-1"})
	// The confirm reply and the pending_reverification goal_update broadcast
	// race each other on the wire; accept them in either order.
	readConfirmExchange(t, r)
	// "Crash": Temporal is unreachable, so the wake failed and the event is
	// still unprocessed.
	ctx := context.Background()
	eventID := "criteria-confirm-" + proposal.Proposal.ID
	events, err := svc.deps.Store.UnprocessedEvents(ctx)
	if err != nil || len(events) != 1 || events[0].ID != eventID || events[0].Status != "pending" {
		t.Fatalf("unprocessed after interrupt: %+v %v", events, err)
	}

	// "Restart": the worker reads unprocessed events and re-delivers them.
	var woke []string
	wake := func(_ context.Context, goalID, id string) error {
		if goalID != "goal-1" {
			t.Errorf("wake goal: %q", goalID)
		}
		woke = append(woke, id)
		return nil
	}
	for _, e := range events {
		if err = wake(ctx, e.GoalID, e.ID); err != nil {
			t.Fatal(err)
		}
		if err = svc.deps.Store.SetEventStatus(ctx, e.ID, "signaled"); err != nil {
			t.Fatal(err)
		}
	}
	if len(woke) != 1 || woke[0] != eventID {
		t.Fatalf("replayed events: %v", woke)
	}
	// The version bumped exactly once and the replayed event keeps its ID.
	snap, err := svc.deps.Store.GetGoalSnapshot(ctx, "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 1 || snap.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("goal after restart: %+v", snap.Goal)
	}
	// Once the workflow marks it processed, no further replay picks it up.
	if err = svc.deps.Store.SetEventStatus(ctx, eventID, "processed"); err != nil {
		t.Fatal(err)
	}
	if events, err = svc.deps.Store.UnprocessedEvents(ctx); err != nil || len(events) != 0 {
		t.Fatalf("unprocessed after processing: %+v %v", events, err)
	}
}
