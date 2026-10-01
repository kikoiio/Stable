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
	"stable/internal/store"
)

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

func TestHistoryReplayAndBroadcast(t *testing.T) {
	_, socket := startService(t, nil)
	a, _ := dial(t, socket)
	b, _ := dial(t, socket)
	ra, rb := bufio.NewReader(a), bufio.NewReader(b)

	// Both clients receive the initial history replay (empty).
	send(t, a, ClientMsg{Op: "say", Goal: "goal-1", Text: "focus on J1"})
	msgA := readMsg(t, ra)
	if msgA.Type != "message" || msgA.Message.Text != "focus on J1" {
		t.Fatalf("client A: %+v", msgA)
	}
	msgB := readMsg(t, rb)
	if msgB.Type != "message" || msgB.Message.Text != "focus on J1" {
		t.Fatalf("client B missed broadcast: %+v", msgB)
	}

	// A reconnecting client replays the full transcript.
	a.Close()
	c, _ := dial(t, socket)
	rc := bufio.NewReader(c)
	replayed := readMsg(t, rc)
	if replayed.Type != "message" || replayed.Message.Text != "focus on J1" {
		t.Fatalf("replay: %+v", replayed)
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
	confirmMsg := readMsg(t, r)
	if confirmMsg.Type != "message" || confirmMsg.Message.Kind != core.MessageKindCriteriaConfirm {
		t.Fatalf("confirm message: %+v", confirmMsg)
	}
	snap, err := svc.deps.Store.GetGoalSnapshot(context.Background(), "goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Goal.CriteriaRevision != 1 || len(snap.Goal.Criteria) != 1 || snap.Goal.Criteria[0].Kind != core.CriterionKindERCClean {
		t.Fatalf("criteria after confirm: %+v", snap.Goal)
	}
	events, _ := svc.deps.Store.PendingEvents(context.Background())
	if len(events) != 1 || events[0].Kind != core.EventKindCriteriaUpdate {
		t.Fatalf("criteria update event: %+v", events)
	}
	if done := readMsg(t, r); done.Type != "done" {
		t.Fatalf("missing completion marker: %+v", done)
	}
	// Confirming twice must be rejected by the proposal state machine.
	send(t, conn, ClientMsg{Op: "confirm", ID: proposal.Proposal.ID, Goal: "goal-1"})
	errMsg := readMsg(t, r)
	if errMsg.Type != "error" {
		t.Fatalf("double confirm: %+v", errMsg)
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
