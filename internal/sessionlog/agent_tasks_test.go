package sessionlog

import (
	"os"
	"strings"
	"testing"
	"time"
)

func newAgentTaskFixture(t *testing.T, work string) (string, string, RunStarted) {
	t.Helper()
	root := t.TempDir()
	s, err := Create(root, "agents")
	if err != nil {
		t.Fatal(err)
	}
	parent := RunStarted{RunID: "parent", WorkKind: work, Intent: "parent"}
	if work == "goal" {
		parent.GoalID, parent.WorkItemID = "goal", "item"
	}
	if _, err := Append(root, s.ID, EventRunStarted, parent); err != nil {
		t.Fatal(err)
	}
	start := parent
	start.RunID, start.Intent = "agent-run", "inspect"
	start.AgentTaskID, start.AgentName, start.OriginRunID = "task-1", "explore", parent.RunID
	return root, s.ID, start
}

func appendAgentTaskEvent(t *testing.T, root, session string, seq uint64, kind string, payload any) Event {
	t.Helper()
	e, err := Append(root, session, EventRunEvent, RunEvent{ID: "event-" + kind + time.Now().Format("150405.000000000"), RunID: "agent-run", SessionID: session, RunSeq: seq, At: time.Now().UTC(), Kind: kind, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func agentTaskPayload(status string) AgentTaskDelegation {
	return AgentTaskDelegation{BatchID: "batch-1", TaskID: "task-1", TaskName: "inspect", Status: status, UpdatedAt: time.Now().UTC()}
}

func TestAgentTaskSourceAndProjection(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "goal")
	started, err := Append(root, session, EventRunStarted, start)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := AgentTasks(transcript)
	if err != nil || len(tasks) != 1 || tasks[0].StartSeq != started.Seq || tasks[0].TerminalSeq != 0 || tasks[0].Started.OriginRunID != "parent" {
		t.Fatalf("initial projection = %+v, %v", tasks, err)
	}
	appendAgentTaskEvent(t, root, session, 1, "delegation_event", agentTaskPayload("queued"))
	appendAgentTaskEvent(t, root, session, 2, "delegation_event", agentTaskPayload("running"))
	terminal := agentTaskPayload("succeeded")
	terminal.Summary = "inspected safely"
	child := appendAgentTaskEvent(t, root, session, 3, "delegation_event", terminal)
	outcome := appendAgentTaskEvent(t, root, session, 4, "terminal", map[string]string{"status": "completed"})
	transcript, err = Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err = AgentTasks(transcript)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("projection = %+v, %v", tasks, err)
	}
	got := tasks[0]
	if got.TerminalSeq != child.Seq || got.LastSeq != outcome.Seq || got.LastRunSeq != 4 || got.RunStatus != "completed" || got.Delegation.Summary != terminal.Summary {
		t.Fatalf("terminal projection = %+v", got)
	}
	tail, err := ReplayAfter(root, session, child.Seq)
	if err != nil || len(tail.Events) != 1 || tail.Events[0].Seq != outcome.Seq {
		t.Fatalf("cursor tail = %+v, %v", tail, err)
	}
}

func TestAgentTaskSourceRejectsInvalidOwnership(t *testing.T) {
	root, session, valid := newAgentTaskFixture(t, "goal")
	cases := []struct {
		name   string
		change func(*RunStarted)
	}{
		{"missing name", func(s *RunStarted) { s.AgentName = "" }},
		{"missing task ID", func(s *RunStarted) { s.AgentTaskID = "" }},
		{"long name", func(s *RunStarted) { s.AgentName = strings.Repeat("a", 65) }},
		{"long ID", func(s *RunStarted) { s.AgentTaskID = strings.Repeat("a", 129) }},
		{"unsafe name", func(s *RunStarted) { s.AgentName = "../explore" }},
		{"unknown origin", func(s *RunStarted) { s.OriginRunID = "missing" }},
		{"self origin", func(s *RunStarted) { s.OriginRunID = s.RunID }},
		{"different goal", func(s *RunStarted) { s.GoalID = "other" }},
		{"different work item", func(s *RunStarted) { s.WorkItemID = "other" }},
		{"fork overlap", func(s *RunStarted) { s.ForkSkill = "skill" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := valid
			tc.change(&bad)
			if _, err := Append(root, session, EventRunStarted, bad); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	valid.OriginRunID = "" // Slash requests have no parent model run.
	if _, err := Append(root, session, EventRunStarted, valid); err != nil {
		t.Fatal(err)
	}
	duplicate := valid
	duplicate.RunID = "other-run"
	if _, err := Append(root, session, EventRunStarted, duplicate); err == nil {
		t.Fatal("duplicate task ID accepted")
	}
}

func TestAgentTaskLifecycleAndTerminalImmutable(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "session")
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	appendRaw := func(seq uint64, kind string, payload any) error {
		_, err := Append(root, session, EventRunEvent, RunEvent{ID: "candidate", RunID: start.RunID, SessionID: session, RunSeq: seq, At: time.Now().UTC(), Kind: kind, Payload: payload})
		return err
	}
	if err := appendRaw(1, "delegation_event", agentTaskPayload("running")); err == nil {
		t.Fatal("running without queued accepted")
	}
	appendAgentTaskEvent(t, root, session, 1, "delegation_event", agentTaskPayload("queued"))
	if err := appendRaw(2, "terminal", map[string]string{"status": "interrupted"}); err == nil {
		t.Fatal("run terminal before child accepted")
	}
	for _, change := range []func(*AgentTaskDelegation){
		func(d *AgentTaskDelegation) { d.TaskID = "other" },
		func(d *AgentTaskDelegation) { d.BatchID = "other" },
		func(d *AgentTaskDelegation) { d.SessionID = "other" },
		func(d *AgentTaskDelegation) { d.Summary = strings.Repeat("a", 8193) },
		func(d *AgentTaskDelegation) { d.Error = strings.Repeat("a", 1025) },
		func(d *AgentTaskDelegation) { d.Status = "unknown" },
	} {
		bad := agentTaskPayload("running")
		change(&bad)
		if err := appendRaw(2, "delegation_event", bad); err == nil {
			t.Fatalf("invalid delegation accepted: %+v", bad)
		}
	}
	appendAgentTaskEvent(t, root, session, 2, "delegation_event", agentTaskPayload("canceled"))
	if err := appendRaw(3, "delegation_event", agentTaskPayload("succeeded")); err == nil {
		t.Fatal("terminal overwrite accepted")
	}
	if err := appendRaw(3, "terminal", map[string]string{"status": "completed"}); err == nil {
		t.Fatal("contradictory outcome accepted")
	}
	appendAgentTaskEvent(t, root, session, 3, "terminal", map[string]string{"status": "cancelled"})
	if err := appendRaw(4, "delegation_event", agentTaskPayload("canceled")); err == nil {
		t.Fatal("task event after run terminal accepted")
	}
}

func TestAgentTaskNotificationOwnershipDedupAndRecovery(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "goal")
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	child := appendAgentTaskEvent(t, root, session, 1, "delegation_event", agentTaskPayload("interrupted"))
	n := AgentTaskNotification{TaskID: start.AgentTaskID, TerminalSeq: child.Seq, DestinationRunID: "unstarted"}
	for _, bad := range []AgentTaskNotification{
		{TaskID: "missing", TerminalSeq: child.Seq, DestinationRunID: "next"},
		{TaskID: start.AgentTaskID, TerminalSeq: child.Seq - 1, DestinationRunID: "next"},
		{TaskID: start.AgentTaskID, TerminalSeq: child.Seq, DestinationRunID: start.RunID},
	} {
		if _, err := Append(root, session, EventAgentTaskNotification, bad); err == nil {
			t.Fatalf("invalid notification accepted: %+v", bad)
		}
	}
	if _, err := Append(root, session, EventAgentTaskNotification, n); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, session, EventAgentTaskNotification, n); err == nil {
		t.Fatal("duplicate notification accepted")
	}
	wrong := RunStarted{RunID: "unstarted", WorkKind: "session", Intent: "new"}
	if _, err := Append(root, session, EventRunStarted, wrong); err == nil {
		t.Fatal("future destination acquired different owner")
	}
	n.DestinationRunID = "next"
	if _, err := Append(root, session, EventAgentTaskNotification, n); err != nil {
		t.Fatalf("unstarted destination prevented replay: %v", err)
	}
	next := RunStarted{RunID: "next", WorkKind: "goal", GoalID: start.GoalID, WorkItemID: start.WorkItemID, Intent: "new"}
	if _, err := Append(root, session, EventRunStarted, next); err != nil {
		t.Fatal(err)
	}
	n.DestinationRunID = "third"
	if _, err := Append(root, session, EventAgentTaskNotification, n); err == nil {
		t.Fatal("delivered terminal handed off twice")
	}
	transcript, err := Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := AgentTaskNotifications(transcript)
	if err != nil || len(notifications) != 2 {
		t.Fatalf("notifications = %+v, %v", notifications, err)
	}
}

func TestAgentTaskReplayRejectsCorruption(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "session")
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	path, err := SessionPath(root, session)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"origin_run_id":"parent"`, `"origin_run_id":"unknown"`, 1))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(root, session); err == nil {
		t.Fatal("corrupt agent owner replayed")
	}
}

func TestAgentTaskRecoveryWithoutQueuedEvent(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "session")
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	appendAgentTaskEvent(t, root, session, 1, "delegation_event", agentTaskPayload("interrupted"))
	appendAgentTaskEvent(t, root, session, 2, "terminal", map[string]string{"status": "interrupted"})
	transcript, err := Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := AgentTasks(transcript)
	if err != nil || len(tasks) != 1 || tasks[0].RunStatus != "interrupted" || tasks[0].TerminalSeq == 0 {
		t.Fatalf("recovered task = %+v, %v", tasks, err)
	}
}

func TestAgentTaskOriginCallAssociationAndConcurrentCalls(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "session")
	for _, id := range []string{"call:first", "call:second"} {
		if _, err := Append(root, session, EventToolCall, ToolCall{RunID: "parent", CallID: id, Name: "run_agent"}); err != nil {
			t.Fatal(err)
		}
	}
	// Both calls are logged before either child starts. Identity, rather than
	// the order of the child starts, controls the recovery association.
	start.OriginCallID = "call:second"
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatal(err)
	}
	second := start
	second.RunID, second.AgentTaskID, second.OriginCallID = "agent-run-2", "task-2", "call:first"
	if _, err := Append(root, session, EventRunStarted, second); err != nil {
		t.Fatal(err)
	}
	transcript, err := Replay(root, session)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := AgentTasks(transcript)
	if err != nil || len(tasks) != 2 || tasks[0].Started.OriginCallID != "call:second" || tasks[1].Started.OriginCallID != "call:first" {
		t.Fatalf("wrong call association: %+v, %v", tasks, err)
	}
	duplicate := start
	duplicate.RunID, duplicate.AgentTaskID = "agent-run-3", "task-3"
	if _, err := Append(root, session, EventRunStarted, duplicate); err == nil {
		t.Fatal("one call started a second task")
	}
	if _, err := Append(root, session, EventToolResult, ToolResult{CallID: "call:second", Result: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, session, EventRunStarted, duplicate); err == nil {
		t.Fatal("closed origin call accepted")
	}
}

func TestAgentTaskOriginCallRejectsInvalidAssociation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   ToolCall
		modify func(*RunStarted)
		close  bool
	}{
		{name: "unknown call", call: ToolCall{CallID: "other", RunID: "parent", Name: "run_agent"}},
		{name: "different owner", call: ToolCall{CallID: "call-1", RunID: "other-parent", Name: "run_agent"}},
		{name: "different tool", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "task_output"}},
		{name: "closed call", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "run_agent"}, close: true},
		{name: "missing parent", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "run_agent"}, modify: func(s *RunStarted) { s.OriginRunID = "" }},
		{name: "ordinary run", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "run_agent"}, modify: func(s *RunStarted) { s.AgentTaskID, s.AgentName, s.OriginRunID = "", "", "" }},
		{name: "oversized call ID", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "run_agent"}, modify: func(s *RunStarted) { s.OriginCallID = strings.Repeat("x", 257) }},
		{name: "control call ID", call: ToolCall{CallID: "call-1", RunID: "parent", Name: "run_agent"}, modify: func(s *RunStarted) { s.OriginCallID = "call\n1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, session, start := newAgentTaskFixture(t, "session")
			if _, err := Append(root, session, EventToolCall, tc.call); err != nil {
				t.Fatal(err)
			}
			if tc.close {
				if _, err := Append(root, session, EventToolResult, ToolResult{CallID: tc.call.CallID, Result: "done"}); err != nil {
					t.Fatal(err)
				}
			}
			start.OriginCallID = "call-1"
			if tc.modify != nil {
				tc.modify(&start)
			}
			if _, err := Append(root, session, EventRunStarted, start); err == nil {
				t.Fatal("invalid call association accepted")
			}
		})
	}
}

func TestAgentTaskOriginCallSupportsLegacyToolOwner(t *testing.T) {
	root, session, start := newAgentTaskFixture(t, "session")
	if _, err := Append(root, session, EventToolCall, ToolCall{CallID: "legacy-call", Name: "run_agent"}); err != nil {
		t.Fatal(err)
	}
	start.OriginCallID = "legacy-call"
	if _, err := Append(root, session, EventRunStarted, start); err != nil {
		t.Fatalf("legacy call ownership was not inferred: %v", err)
	}
}
