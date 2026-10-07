package conversation

import (
	"encoding/json"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

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
