package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestAgentTaskTUIReconnectReplaysFromIndependentCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	socketDir, err := os.MkdirTemp("/tmp", "m09-agent-reconnect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")

	childRunner := &reconnectAgentChildRunner{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(childRunner.finish)
	reporter := conversation.NewDelegationEventReporter()
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, reporter)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := conversation.NewAgentTaskCoordinator()
	t.Cleanup(func() {
		coordinator.Close()
		pool.Close()
	})
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, AgentTasks: coordinator, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Bind(svc)
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	created, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parentStream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "keep parent run active while task stream reconnects", Messages: []llm.Message{{Role: "user", Content: "Keep the parent run active."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() {
		parentRun.finish(agent.RunCompleted)
		_ = parentStream.Close()
	})
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start parent run: message=%+v err=%v", started, err)
	}

	taskMessages, err := conversation.Request(ctx, socket, conversation.ClientMsg{
		Op: "agent_task_start", SessionID: sessionID, AgentName: "explore", Text: "inspect reconnect behavior",
	})
	if err != nil || len(taskMessages) == 0 || taskMessages[0].AgentTask == nil {
		t.Fatalf("start background task: messages=%+v err=%v", taskMessages, err)
	}
	task := *taskMessages[0].AgentTask
	select {
	case <-childRunner.started:
	case <-ctx.Done():
		t.Fatal("background task runner did not start")
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model.LastCursor, model.stream = 41, parentStream
	updated, command := model.handleAgentResult(resultMsg{op: "agent_task_start", sessionID: sessionID, msgs: taskMessages})
	model = updated.(Model)
	if command == nil {
		t.Fatal("task response did not establish the independent task stream")
	}
	startedMsg, ok := command().(agentStreamStartedMsg)
	if !ok || startedMsg.err != nil {
		t.Fatalf("connect first task stream: message=%+v", startedMsg)
	}
	updated, receive := model.Update(startedMsg)
	model = updated.(Model)
	if receive == nil || model.agentStream != startedMsg.client {
		t.Fatal("first task stream was not installed")
	}
	var firstCursor uint64
	for {
		message := receiveAgentReconnectMessage(t, ctx, receive)
		if message.err != nil {
			t.Fatalf("receive initial task progress: %v", message.err)
		}
		updated, receive = model.Update(message)
		model = updated.(Model)
		if message.message.Type == "run_event" && message.message.RunEvent != nil && message.message.RunEvent.RunID == task.RunID && message.message.RunEvent.Kind == string(agent.EventDelegation) {
			var progress agent.DelegationEvent
			if err := decodeEventData(message.message.RunEvent.Payload, &progress); err != nil {
				t.Fatal(err)
			}
			if progress.Status == agent.DelegationRunning {
				firstCursor = message.message.Cursor
				break
			}
		}
	}
	if firstCursor == 0 || model.agentCursor != firstCursor {
		t.Fatalf("task stream cursor=%d, model cursor=%d", firstCursor, model.agentCursor)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != 41 || model.stream != parentStream {
		t.Fatal("first task stream changed parent run identity, cursor, or stream")
	}
	if err := startedMsg.client.Close(); err != nil {
		t.Fatal(err)
	}
	updated, retry := model.Update(agentStreamMsg{client: startedMsg.client, sessionID: sessionID, generation: model.agentGeneration, err: io.EOF})
	model = updated.(Model)
	if retry == nil || model.agentStream != nil || model.agentCursor != firstCursor {
		t.Fatal("task stream disconnect did not retain its independent cursor for retry")
	}

	childRunner.finish()
	terminalMessages, err := conversation.Request(ctx, socket, conversation.ClientMsg{
		Op: "agent_task_get", SessionID: sessionID, TaskID: task.ID, WaitMS: 3000,
	})
	if err != nil || len(terminalMessages) == 0 || terminalMessages[0].AgentTask == nil || terminalMessages[0].AgentTask.Status != agent.DelegationSucceeded {
		t.Fatalf("wait for task terminal: messages=%+v err=%v", terminalMessages, err)
	}
	terminalCursor := terminalMessages[0].AgentTask.Cursor
	if terminalCursor <= firstCursor {
		t.Fatalf("terminal cursor %d did not advance past consumed cursor %d", terminalCursor, firstCursor)
	}

	updated, reconnect := model.Update(agentReconnectMsg{sessionID: sessionID, generation: model.agentGeneration})
	model = updated.(Model)
	batchResult := reconnect()
	batch, ok := batchResult.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("reconnect did not schedule stream plus durable task refresh: %T", batchResult)
	}
	startedMsg, ok = batch[0]().(agentStreamStartedMsg)
	if !ok || startedMsg.err != nil {
		t.Fatalf("reconnect task stream: message=%+v", startedMsg)
	}
	updated, receive = model.Update(startedMsg)
	model = updated.(Model)
	for model.AgentTasks[0].Status != agent.DelegationSucceeded || model.agentCursor < terminalCursor {
		message := receiveAgentReconnectMessage(t, ctx, receive)
		if message.err != nil {
			t.Fatalf("replay task terminal after reconnect: %v", message.err)
		}
		if message.message.Cursor != 0 && message.message.Cursor <= firstCursor {
			t.Fatalf("reconnect replayed cursor %d already consumed through %d", message.message.Cursor, firstCursor)
		}
		updated, receive = model.Update(message)
		model = updated.(Model)
	}
	// The durable list refresh is intentionally processed after stream replay;
	// it must not render a second copy of the terminal notification.
	refresh, ok := batch[1]().(resultMsg)
	if !ok {
		t.Fatalf("reconnect task refresh result type=%T", refresh)
	}
	updated, _ = model.Update(refresh)
	model = updated.(Model)
	if len(model.AgentTasks) != 1 || model.AgentTasks[0].ID != task.ID || model.AgentTasks[0].Status != agent.DelegationSucceeded || model.AgentTasks[0].Cursor != terminalCursor {
		t.Fatalf("reconnected task snapshot=%+v", model.AgentTasks)
	}
	terminalNotes := 0
	for _, event := range model.Events {
		if event.Type != sessionlog.EventMessage {
			continue
		}
		if note, ok := event.Data.(sessionlog.Message); ok && strings.Contains(note.Text, "reconnect-safe findings") {
			terminalNotes++
		}
	}
	if terminalNotes != 1 {
		t.Fatalf("terminal task summary was presented %d times, want once", terminalNotes)
	}
	if model.agentCursor != terminalCursor {
		t.Fatalf("resumed task cursor=%d, want terminal cursor %d", model.agentCursor, terminalCursor)
	}
	if !model.Pending || model.ActiveRunID != parentRunID || model.LastCursor != 41 || model.stream != parentStream {
		t.Fatal("task stream reconnect changed parent run identity, cursor, or stream")
	}
	_ = startedMsg.client.Close()
}

type reconnectAgentChildRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *reconnectAgentChildRunner) Run(ctx context.Context, _ agent.ChildRunInput) agent.ChildRunResult {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	select {
	case <-r.release:
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "reconnect-safe findings"}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	}
}

func (r *reconnectAgentChildRunner) finish() { r.once.Do(func() { close(r.release) }) }

func receiveAgentReconnectMessage(t *testing.T, ctx context.Context, command tea.Cmd) agentStreamMsg {
	t.Helper()
	result := make(chan tea.Msg, 1)
	go func() { result <- command() }()
	select {
	case message := <-result:
		got, ok := message.(agentStreamMsg)
		if !ok {
			t.Fatalf("agent stream receive type=%T", message)
		}
		return got
	case <-ctx.Done():
		t.Fatalf("timed out waiting for task stream: %v", ctx.Err())
		return agentStreamMsg{}
	}
}
