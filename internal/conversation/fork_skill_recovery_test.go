package conversation

import (
	"encoding/json"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
)

func TestRecoverOpenSlashForkAsInterruptedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "slash fork recovery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: "fork-run", WorkKind: string(agent.WorkSession), Intent: "fork skill review",
		ForkSkill: "review", ForkEntry: sessionlog.SkillEntrySlash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: "queued", RunID: "fork-run", SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: agent.DelegationEvent{
			BatchID: "batch", TaskID: "task", TaskName: "review", Status: agent.DelegationRunning,
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
		t.Fatalf("recovery is not idempotent: first=%d second=%d", len(first.Events), len(second.Events))
	}
	var interrupted, terminal int
	for _, event := range second.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var run sessionlog.RunEvent
		if err = decodeSessionData(event.Data, &run); err != nil {
			t.Fatal(err)
		}
		if run.RunID != "fork-run" {
			continue
		}
		switch run.Kind {
		case string(agent.EventDelegation):
			var item agent.DelegationEvent
			if err = decodeSessionData(run.Payload, &item); err != nil {
				t.Fatal(err)
			}
			if item.Status == agent.DelegationInterrupted {
				interrupted++
			}
		case string(agent.EventTerminal):
			var outcome struct {
				Status agent.RunStatus
			}
			if err = decodeSessionData(run.Payload, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.Status == agent.RunInterrupted {
				terminal++
			}
		}
	}
	if interrupted != 1 || terminal != 1 {
		t.Fatalf("interrupted child events=%d terminal events=%d", interrupted, terminal)
	}
}

func TestRecoverOpenForkLoadSkillPairsToolResult(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "tool fork recovery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: "parent-run", WorkKind: string(agent.WorkSession), Intent: "parent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventSkillInvoked, sessionlog.SkillInvoked{
		Name: "review", Source: "project", Entry: sessionlog.SkillEntryTool,
		Mode: sessionlog.SkillModeFork, RunID: "parent-run",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventToolCall, sessionlog.ToolCall{
		CallID: "load-call", Name: "load_skill", Input: map[string]any{"name": "review"},
	}); err != nil {
		t.Fatal(err)
	}
	if err = recoverDelegationRuns(root); err != nil {
		t.Fatal(err)
	}
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var results, terminals int
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventToolResult {
			continue
		}
		var result sessionlog.ToolResult
		if err = decodeSessionData(event.Data, &result); err != nil {
			t.Fatal(err)
		}
		if result.CallID != "load-call" {
			continue
		}
		results++
		encoded, ok := result.Result.(string)
		if !ok {
			t.Fatalf("fork result type = %T", result.Result)
		}
		var forkResult ForkSkillResult
		if err = json.Unmarshal([]byte(encoded), &forkResult); err != nil {
			t.Fatal(err)
		}
		if forkResult.Status != agent.DelegationInterrupted || forkResult.Error == "" {
			t.Fatalf("fork result = %+v", forkResult)
		}
	}
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var run sessionlog.RunEvent
		if err = decodeSessionData(event.Data, &run); err != nil {
			t.Fatal(err)
		}
		if run.RunID == "parent-run" && run.Kind == string(agent.EventTerminal) {
			terminals++
		}
	}
	if results != 1 || terminals != 1 {
		t.Fatalf("tool results=%d parent terminals=%d", results, terminals)
	}
}
