package conversation

import (
	"encoding/json"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func TestAppendParentDelegationEventAfterTerminalRun(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "run-end-hook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-end", WorkKind: string(agent.WorkSession), Intent: "finish"}); err != nil {
		t.Fatal(err)
	}
	first := sessionlog.RunEvent{ID: "first", RunID: "run-end", SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: map[string]any{"status": agent.RunCompleted}}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, first); err != nil {
		t.Fatal(err)
	}
	service := &Service{deps: Deps{ProjectRoot: root}}
	if err = service.appendParentDelegationEvent("run-end", agent.DelegationEvent{
		SessionID: session.ID, BatchID: "batch", TaskID: "hook", TaskName: "hook agent: exit", Status: agent.DelegationSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(event.Data, &runEvent) == nil && runEvent.RunID == "run-end" && runEvent.RunSeq == 2 && runEvent.Kind == string(agent.EventDelegation) {
			found = true
		}
	}
	if !found {
		t.Fatalf("run_end delegation was not appended after terminal sequence: %+v", transcript.Events)
	}
}

func TestRecoverDelegationClosesOpenToolCallAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-1", WorkKind: string(agent.WorkSession), Intent: "delegate"}); err != nil {
		t.Fatal(err)
	}
	callInput := map[string]any{"tasks": []any{
		map[string]any{"id": "a", "name": "inspect", "instruction": "inspect source"},
		map[string]any{"id": "b", "name": "inspect queued", "instruction": "inspect tests"},
	}}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventToolCall, sessionlog.ToolCall{CallID: "delegate-call", Name: "delegate_tasks", Input: callInput}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "queued", RunID: "run-1", SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: agent.DelegationEvent{BatchID: "batch-1", TaskID: "a", TaskName: "inspect", Status: agent.DelegationRunning},
	}); err != nil {
		t.Fatal(err)
	}
	if err = recoverDelegationRuns(root); err != nil {
		t.Fatal(err)
	}
	first, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = recoverDelegationRuns(root); err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != len(first.Events) {
		t.Fatalf("second startup recovery appended duplicate events: before=%d after=%d", len(first.Events), len(second.Events))
	}
	var interrupted int
	var toolResults int
	var terminal bool
	for _, event := range second.Events {
		switch event.Type {
		case sessionlog.EventToolResult:
			var result sessionlog.ToolResult
			if decodeSessionData(event.Data, &result) == nil && result.CallID == "delegate-call" {
				toolResults++
				encoded, _ := result.Result.(string)
				var values []agent.DelegationResult
				if json.Unmarshal([]byte(encoded), &values) != nil || len(values) != 2 || values[0].Status != agent.DelegationInterrupted || values[1].Status != agent.DelegationInterrupted {
					t.Fatalf("tool result=%s", encoded)
				}
			}
		case sessionlog.EventRunEvent:
			var run sessionlog.RunEvent
			if decodeSessionData(event.Data, &run) != nil {
				continue
			}
			if run.Kind == string(agent.EventDelegation) {
				var update agent.DelegationEvent
				if decodeSessionData(run.Payload, &update) == nil && update.Status == agent.DelegationInterrupted {
					interrupted++
				}
			}
			if run.Kind == string(agent.EventTerminal) {
				var result struct {
					Status agent.RunStatus `json:"status"`
				}
				if decodeSessionData(run.Payload, &result) == nil && result.Status == agent.RunInterrupted {
					terminal = true
				}
			}
		}
	}
	if interrupted != 2 || toolResults != 1 || !terminal {
		t.Fatalf("interrupted=%d toolResults=%d terminal=%v events=%+v", interrupted, toolResults, terminal, second.Events)
	}
}

func TestRecoverInterruptedRunEndHookChildAfterTerminal(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "hook-agent-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "run-end", WorkKind: string(agent.WorkSession), Intent: "finish"}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "terminal", RunID: "run-end", SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(),
		Kind: string(agent.EventTerminal), Payload: map[string]any{"status": agent.RunCompleted},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "hook-running", RunID: "run-end", SessionID: session.ID, RunSeq: 2, At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: agent.DelegationEvent{
			BatchID: "hook-batch", TaskID: "hook-task", TaskName: "hook agent: notify", Status: agent.DelegationRunning,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err = recoverDelegationRuns(root); err != nil {
		t.Fatal(err)
	}
	first, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = recoverDelegationRuns(root); err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != len(first.Events) {
		t.Fatalf("second recovery appended duplicate events: before=%d after=%d", len(first.Events), len(second.Events))
	}
	var interrupted int
	for _, event := range second.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if decodeSessionData(event.Data, &runEvent) != nil || runEvent.Kind != string(agent.EventDelegation) {
			continue
		}
		var update agent.DelegationEvent
		if decodeSessionData(runEvent.Payload, &update) == nil && update.TaskID == "hook-task" && update.Status == agent.DelegationInterrupted {
			interrupted++
		}
	}
	if interrupted != 1 {
		t.Fatalf("expected one interrupted hook child event, got %d: %+v", interrupted, second.Events)
	}
}
