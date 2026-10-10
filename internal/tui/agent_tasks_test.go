package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/platform/ipc"
	"stable/internal/sessionlog"
)

func agentSocketFixture(t *testing.T, responses ...conversation.ServerMsg) (string, <-chan conversation.ClientMsg) {
	t.Helper()
	// Use a short socket path even when the test's full name is long.
	dir, err := os.MkdirTemp("", "stable-tui-agents-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s.sock")
	listener, err := ipc.ListenPrivate(socket, false)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); os.RemoveAll(dir) })
	requests := make(chan conversation.ClientMsg, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var request conversation.ClientMsg
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		requests <- request
		encoder := json.NewEncoder(conn)
		for _, response := range responses {
			if encoder.Encode(response) != nil {
				return
			}
		}
		_ = encoder.Encode(conversation.ServerMsg{Type: "done"})
	}()
	return socket, requests
}

func agentFixtureRequest(t *testing.T, requests <-chan conversation.ClientMsg) conversation.ClientMsg {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("no agent socket request")
		return conversation.ClientMsg{}
	}
}

func agentTUIFixture(t *testing.T) Model {
	t.Helper()
	m := New("socket", t.TempDir())
	m.ActiveSession = "session"
	m.prepareAgentSession()
	return m
}

func TestAgentCommandsUseServerOwnedScopeAndPreserveParent(t *testing.T) {
	for _, tc := range []struct {
		line, operation, name, instruction, task, isolation string
		after                                               uint64
	}{
		{line: "/agents", operation: "agent_list"},
		{line: "/agents reload", operation: "agent_reload"},
		{line: "/agent explore inspect\n  the architecture", operation: "agent_task_start", name: "explore", instruction: "inspect\n  the architecture"},
		{line: "/agent --worktree general-purpose edit the file", operation: "agent_task_start", name: "general-purpose", instruction: "edit the file", isolation: "worktree"},
		{line: "/agent general-purpose explain --worktree syntax", operation: "agent_task_start", name: "general-purpose", instruction: "explain --worktree syntax"},
		{line: "/tasks", operation: "agent_task_list"},
		{line: "/tasks next", operation: "agent_task_list", after: 41},
		{line: "/tasks get task-1", operation: "agent_task_get", task: "task-1"},
		{line: "/tasks stop task-2", operation: "agent_task_cancel", task: "task-2"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			m := agentTUIFixture(t)
			socket, requests := agentSocketFixture(t)
			m.Socket = socket
			m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 9
			m.agentListCursor = 41
			m.Composer.SetValue(tc.line)
			updated, command := m.submitComposer()
			got := updated.(Model)
			if command == nil || !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 9 {
				t.Fatalf("command changed parent state: %+v", got)
			}
			if response := command().(resultMsg); response.err != nil {
				t.Fatal(response.err)
			}
			request := agentFixtureRequest(t, requests)
			if request.Isolation != tc.isolation || request.Op != tc.operation || request.SessionID != "session" || request.ProjectRoot != "" || request.RunID != "" || request.Run != nil || request.AgentName != tc.name || request.Text != tc.instruction || request.TaskID != tc.task || request.AfterSeq != tc.after {
				t.Fatalf("command request=%+v", request)
			}
			if request.Op == "agent_task_start" && !request.Background {
				t.Fatal("slash agent task must be independent background work")
			}
		})
	}
}

func TestAgentCommandsRejectInvalidUsage(t *testing.T) {
	for _, line := range []string{"/agents unknown", "/agent", "/agent explore", "/agent --worktree", "/agent --worktree builder", "/tasks invalid", "/tasks stop", "/tasks get a b"} {
		m := agentTUIFixture(t)
		m.Composer.SetValue(line)
		updated, command := m.submitComposer()
		if command != nil || !strings.Contains(updated.(Model).Status, "用法") {
			t.Fatalf("invalid command accepted: %s", line)
		}
	}
	m := agentTUIFixture(t)
	m.ActiveSession = ""
	m.Composer.SetValue("/agent explore inspect")
	updated, command := m.submitComposer()
	if command != nil || !strings.Contains(updated.(Model).Status, "会话") {
		t.Fatal("agent command without a session accepted")
	}
}

func TestAgentCatalogDisplaysOnlyPublicMetadataAndRejections(t *testing.T) {
	m := agentTUIFixture(t)
	updated, _ := m.handleResult(resultMsg{op: "agent_reload", sessionID: "session", msgs: []conversation.ServerMsg{{Type: "agent_reload", Agents: &agentcatalog.Snapshot{
		Definitions: []agentcatalog.Metadata{{Name: "explore", Description: "Inspect architecture", Model: "inherit", Source: "project", Tools: []string{"read_file", "grep"}, MaxTurns: 3, ReadOnly: true}},
		Rejections:  []string{"invalid.md: unsupported field permissionMode"},
	}}}})
	got := updated.(Model)
	out := projectTranscript(got.Events, 100, false)
	for _, want := range []string{"只读", "explore", "project", "inherit", "3", "read_file", "grep", "跳过", "permissionMode"} {
		if !strings.Contains(out, want) {
			t.Fatalf("catalog missing %q: %s", want, out)
		}
	}
	if !strings.Contains(got.Status, "重载") {
		t.Fatal("reload result was not shown")
	}
}

func TestAgentOneShotResultsDoNotEndParentAndStopWaitsForExit(t *testing.T) {
	m := agentTUIFixture(t)
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 31
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	task := agent.AgentTaskSnapshot{ID: "task", RunID: "child", SessionID: "session", AgentName: "explore", Status: agent.DelegationRunning, Stage: "inspect", Cursor: 50}
	updated, _ := m.handleResult(resultMsg{op: "agent_task_cancel", sessionID: "session", msgs: []conversation.ServerMsg{{Type: "agent_task_cancel", AgentTask: &task}}})
	got := updated.(Model)
	if !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 31 || got.stream != parentStream {
		t.Fatal("task response replaced the parent run")
	}
	if len(got.AgentTasks) != 1 || got.AgentTasks[0].Status != agent.DelegationRunning || !strings.Contains(got.Status, "等待任务实际退出") {
		t.Fatalf("cancel falsely reported task exit: tasks=%+v status=%s", got.AgentTasks, got.Status)
	}
	updated, _ = got.handleResult(resultMsg{op: "agent_task_get", sessionID: "other", msgs: []conversation.ServerMsg{{Type: "agent_task_get", AgentTask: &task}}})
	if updated.(Model).Status != got.Status {
		t.Fatal("stale session response changed current session")
	}
}

func TestAgentTerminalUpdatesDeduplicateAcrossBothStreams(t *testing.T) {
	m := agentTUIFixture(t)
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 11
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	child := agent.AgentTaskSnapshot{ID: "task", RunID: "child", SessionID: "session", AgentName: "explore", Status: agent.DelegationSucceeded, Summary: "safe result", Cursor: 20}
	updated, _ := m.Update(runStreamMsg{client: parentStream, message: conversation.ServerMsg{Type: "agent_task_update", RunID: "child", AgentTask: &child}})
	m = updated.(Model)
	if !m.Pending || m.ActiveRunID != "parent" || m.LastCursor != 11 || len(m.AgentTasks) != 1 || len(m.Events) != 1 {
		t.Fatalf("parent stream mishandled task terminal: %+v", m)
	}
	m.agentStream = &conversation.StreamClient{}
	updated, _ = m.Update(agentStreamMsg{client: m.agentStream, sessionID: "session", generation: m.agentGeneration, message: conversation.ServerMsg{Type: "agent_task_update", AgentTask: &child, Cursor: 20}})
	m = updated.(Model)
	if len(m.Events) != 1 || m.agentCursor != 20 || m.LastCursor != 11 {
		t.Fatal("duplicate notification or shared cursor")
	}
	child.Cursor = 21 // The independent run outcome can follow child terminal.
	m.applyAgentTask(child, true)
	if len(m.Events) != 1 || m.AgentTasks[0].Cursor != 21 {
		t.Fatal("run outcome repeated an unchanged task terminal")
	}
}

func TestAgentStreamShowsSafeProgressAndIgnoresParentText(t *testing.T) {
	m := agentTUIFixture(t)
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 8
	m.AgentTasks = []agent.AgentTaskSnapshot{{ID: "task", RunID: "child", SessionID: "session", AgentName: "explore", Status: agent.DelegationQueued, Cursor: 4}}
	client := &conversation.StreamClient{}
	m.agentStream = client
	progress := agent.DelegationEvent{TaskID: "task", Status: agent.DelegationRunning, Stage: "read", Summary: "found files"}
	updated, _ := m.Update(agentStreamMsg{client: client, sessionID: "session", generation: m.agentGeneration, message: conversation.ServerMsg{Type: "run_event", Cursor: 9, RunEvent: &sessionlog.RunEvent{RunID: "child", SessionID: "session", Kind: string(agent.EventDelegation), Payload: progress}}})
	m = updated.(Model)
	if m.AgentTasks[0].Stage != "read" || len(m.Events) != 1 || m.agentCursor != 9 || !m.Pending || m.ActiveRunID != "parent" || m.LastCursor != 8 {
		t.Fatal("independent progress changed parent state or omitted stage")
	}
	updated, _ = m.Update(agentStreamMsg{client: client, sessionID: "session", generation: m.agentGeneration, message: conversation.ServerMsg{Type: "run_event", Cursor: 10, RunEvent: &sessionlog.RunEvent{RunID: "parent", SessionID: "session", Kind: "thinking_delta", Payload: map[string]string{"text": "parent private thoughts"}}}})
	m = updated.(Model)
	if len(m.Events) != 1 || m.agentCursor != 10 || m.LastCursor != 8 {
		t.Fatal("task stream displayed raw thinking or changed parent cursor")
	}
}

func TestAgentSessionSubscriptionResumesOwnCursor(t *testing.T) {
	m := agentTUIFixture(t)
	m.agentCursor, m.LastCursor, m.ActiveRunID, m.Pending = 19, 50, "parent", true
	socket, requests := agentSocketFixture(t)
	m.Socket = socket
	command := m.ensureAgentStream()
	if command == nil {
		t.Fatal("missing session task subscription")
	}
	message := command().(agentStreamStartedMsg)
	if message.err != nil {
		t.Fatal(message.err)
	}
	defer message.client.Close()
	request := agentFixtureRequest(t, requests)
	if request.Op != "run_subscribe" || request.SessionID != "session" || request.RunID != "" || request.AfterSeq != 19 {
		t.Fatalf("subscription borrowed parent identity/cursor: %+v", request)
	}
	updated, receive := m.Update(message)
	got := updated.(Model)
	if receive == nil || got.agentStream != message.client || got.stream != nil || !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 50 {
		t.Fatal("task subscription replaced parent stream")
	}
}

func TestAgentRestoreRebuildsDurableTaskAndKeepsParentCursor(t *testing.T) {
	m := agentTUIFixture(t)
	m.LastCursor = 99
	m.Events = []sessionlog.Event{
		{SessionID: "session", Seq: 2, Type: sessionlog.EventRunStarted, Data: sessionlog.RunStarted{RunID: "child", WorkKind: "session", Intent: "role task", AgentTaskID: "task", AgentName: "explore"}},
		{SessionID: "session", Seq: 3, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{ID: "queued", RunID: "child", SessionID: "session", RunSeq: 1, Kind: "delegation_event", Payload: sessionlog.AgentTaskDelegation{BatchID: "batch", TaskID: "task", TaskName: "explore", Status: "queued"}}},
		{SessionID: "session", Seq: 4, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{ID: "finished", RunID: "child", SessionID: "session", RunSeq: 2, Kind: "delegation_event", Payload: sessionlog.AgentTaskDelegation{BatchID: "batch", TaskID: "task", TaskName: "explore", Status: "succeeded", Summary: "restored summary"}}},
		{SessionID: "session", Seq: 5, Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{ID: "outcome", RunID: "child", SessionID: "session", RunSeq: 3, Kind: "terminal", Payload: map[string]string{"status": "completed"}}},
	}
	if command := m.restoreAgentTasks(); command == nil {
		t.Fatal("restore omitted task query/subscription")
	}
	if len(m.AgentTasks) != 1 || m.AgentTasks[0].Status != agent.DelegationSucceeded || m.AgentTasks[0].Summary != "restored summary" || m.agentCursor != 5 || m.LastCursor != 99 {
		t.Fatalf("restore state=%+v cursor=%d parent=%d", m.AgentTasks, m.agentCursor, m.LastCursor)
	}
	// Stale connections and retry timers from a previous session do nothing.
	updated, command := m.Update(agentReconnectMsg{sessionID: "other", generation: m.agentGeneration})
	if command != nil || updated.(Model).agentCursor != 5 {
		t.Fatal("stale reconnect changed current task subscription")
	}
	updated, _ = m.handleResult(resultMsg{op: "agent_task_restore", sessionID: "session", err: errors.New("offline")})
	if updated.(Model).LastCursor != 99 {
		t.Fatal("task restore failure reset parent cursor")
	}
}

func TestAgentTaskTranscriptNeverDisplaysRawChildTextOrThinking(t *testing.T) {
	events := []sessionlog.Event{
		{Type: sessionlog.EventRunStarted, Data: sessionlog.RunStarted{RunID: "child", AgentTaskID: "task", AgentName: "explore"}},
		{Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "child", Kind: "thinking_delta", Payload: map[string]string{"text": "private child thinking"}}},
		{Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "child", Kind: "text_delta", Payload: map[string]string{"text": "raw child transcript"}}},
		{Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "child", Kind: "delegation_event", Payload: agent.DelegationEvent{BatchID: "batch", TaskID: "task", TaskName: "explore", Status: agent.DelegationSucceeded, Summary: "public child summary"}}},
		{Type: sessionlog.EventRunEvent, Data: sessionlog.RunEvent{RunID: "parent", Kind: "text_delta", Payload: map[string]string{"text": "parent response"}}},
	}
	out := projectTranscript(events, 100, false)
	if strings.Contains(out, "private child thinking") || strings.Contains(out, "raw child transcript") || !strings.Contains(out, "public child summary") || !strings.Contains(out, "parent response") {
		t.Fatalf("unsafe child transcript projection: %s", out)
	}
}
