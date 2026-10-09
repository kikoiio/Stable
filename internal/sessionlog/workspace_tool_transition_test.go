package sessionlog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func workspaceTransitionFixture(t *testing.T, runID, callID, toolName, action string) (string, SessionInfo, WorkspaceToolTransition) {
	t.Helper()
	root := t.TempDir()
	session, err := Create(root, "workspace tool transition")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, session.ID, EventRunStarted, RunStarted{RunID: runID, WorkKind: "session", Intent: "workspace lifecycle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(root, session.ID, EventToolCall, ToolCall{RunID: runID, CallID: callID, Name: toolName}); err != nil {
		t.Fatal(err)
	}
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	transition := WorkspaceToolTransition{
		ID: id, SessionID: session.ID, RunID: runID, CallID: callID, WorkKind: "session",
		Action: action, Status: WorkspaceToolTransitionPending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if action == "enter" || action == "export" {
		transition.WorkspaceID, err = NewID()
		if err != nil {
			t.Fatal(err)
		}
	}
	return root, session, transition
}

func appendWorkspaceTransitionTerminal(t *testing.T, root string, session SessionInfo, transition WorkspaceToolTransition, status string) error {
	t.Helper()
	transition.Status = status
	transition.UpdatedAt = time.Now().UTC()
	_, err := Append(root, session.ID, EventWorkspaceToolTransition, transition)
	return err
}

func appendWorkspaceRunTerminal(t *testing.T, root string, session SessionInfo, runID string) {
	t.Helper()
	_, err := Append(root, session.ID, EventRunEvent, RunEvent{
		ID: "terminal-" + runID, RunID: runID, SessionID: session.ID, RunSeq: 1,
		At: time.Now().UTC(), Kind: "terminal", Payload: map[string]string{"status": "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceTransitionRequiresExactLifecycleToolCall(t *testing.T) {
	t.Run("action name mismatch", func(t *testing.T) {
		root, _, transition := workspaceTransitionFixture(t, "lead-1", "call-1", "exit_worktree", "enter")
		if _, err := Append(root, transition.SessionID, EventWorkspaceToolTransition, transition); err == nil || !strings.Contains(err.Error(), "action does not match") {
			t.Fatalf("mismatched action/tool call error=%v", err)
		}
	})
	t.Run("call belongs to another run", func(t *testing.T) {
		root := t.TempDir()
		session, err := Create(root, "cross run call")
		if err != nil {
			t.Fatal(err)
		}
		startRun(t, root, session.ID, "lead-1")
		startRun(t, root, session.ID, "lead-2")
		if _, err := Append(root, session.ID, EventToolCall, ToolCall{RunID: "lead-2", CallID: "shared-call", Name: "exit_worktree"}); err != nil {
			t.Fatal(err)
		}
		transition := testWorkspaceTransition(t, session.ID, "lead-1", "shared-call", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err == nil || !strings.Contains(err.Error(), "belongs to another run") {
			t.Fatalf("cross-run call error=%v", err)
		}
	})
	t.Run("completed call ID may be reused by a later run", func(t *testing.T) {
		root := t.TempDir()
		session, err := Create(root, "reused call ID")
		if err != nil {
			t.Fatal(err)
		}
		startRun(t, root, session.ID, "lead-1")
		if _, err := Append(root, session.ID, EventToolCall, ToolCall{RunID: "lead-1", CallID: "shared-call", Name: "exit_worktree"}); err != nil {
			t.Fatal(err)
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: "shared-call", Result: "done"}); err != nil {
			t.Fatal(err)
		}
		startRun(t, root, session.ID, "lead-2")
		if _, err := Append(root, session.ID, EventToolCall, ToolCall{RunID: "lead-2", CallID: "shared-call", Name: "exit_worktree"}); err != nil {
			t.Fatal(err)
		}
		transition := testWorkspaceTransition(t, session.ID, "lead-2", "shared-call", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatalf("sequential CallID reuse was not paired with the later run: %v", err)
		}
		if _, err := Replay(root, session.ID); err != nil {
			t.Fatalf("sequential CallID reuse failed replay: %v", err)
		}
	})
}

func TestWorkspaceTransitionTerminalStatesRequireMatchingEvidence(t *testing.T) {
	t.Run("applied needs successful result and terminal", func(t *testing.T) {
		root, session, transition := workspaceTransitionFixture(t, "lead-applied", "call-applied", "exit_worktree", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatal(err)
		}
		if err := appendWorkspaceTransitionTerminal(t, root, session, transition, WorkspaceToolTransitionApplied); err == nil {
			t.Fatal("applied transition without terminal or result accepted")
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: transition.CallID, Result: "scheduled"}); err != nil {
			t.Fatal(err)
		}
		if err := appendWorkspaceTransitionTerminal(t, root, session, transition, WorkspaceToolTransitionApplied); err == nil {
			t.Fatal("applied transition without run terminal accepted")
		}
		appendWorkspaceRunTerminal(t, root, session, transition.RunID)
		if err := appendWorkspaceTransitionTerminal(t, root, session, transition, WorkspaceToolTransitionApplied); err != nil {
			t.Fatalf("valid applied transition rejected: %v", err)
		}
		if _, err := Replay(root, session.ID); err != nil {
			t.Fatalf("valid applied history failed replay: %v", err)
		}
	})
	t.Run("interrupted cannot discard successful terminal call", func(t *testing.T) {
		root, session, transition := workspaceTransitionFixture(t, "lead-interrupted", "call-interrupted", "exit_worktree", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatal(err)
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: transition.CallID, Result: "scheduled"}); err != nil {
			t.Fatal(err)
		}
		appendWorkspaceRunTerminal(t, root, session, transition.RunID)
		transition.Status, transition.Error, transition.UpdatedAt = WorkspaceToolTransitionInterrupted, "must not apply", time.Now().UTC()
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err == nil {
			t.Fatal("interrupted transition with successful terminal call accepted")
		}
	})
	t.Run("restart may interrupt before run terminal", func(t *testing.T) {
		root, session, transition := workspaceTransitionFixture(t, "lead-restart", "call-restart", "exit_worktree", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatal(err)
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: transition.CallID, Result: "scheduled"}); err != nil {
			t.Fatal(err)
		}
		transition.Status, transition.Error, transition.UpdatedAt = WorkspaceToolTransitionInterrupted, "service restarted", time.Now().UTC()
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatalf("valid pre-terminal restart interruption rejected: %v", err)
		}
		if _, err := Replay(root, session.ID); err != nil {
			t.Fatalf("valid interrupted history failed replay: %v", err)
		}
	})
	t.Run("terminal failed tool may be interrupted without applying", func(t *testing.T) {
		root, session, transition := workspaceTransitionFixture(t, "lead-no-result", "call-no-result", "exit_worktree", "exit")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatal(err)
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: transition.CallID, Error: "tool failed"}); err != nil {
			t.Fatal(err)
		}
		appendWorkspaceRunTerminal(t, root, session, transition.RunID)
		transition.Status, transition.Error, transition.UpdatedAt = WorkspaceToolTransitionInterrupted, "transition was not applied", time.Now().UTC()
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatalf("valid failed-tool interruption rejected: %v", err)
		}
		if _, err := Replay(root, session.ID); err != nil {
			t.Fatalf("valid failed-tool interruption failed replay: %v", err)
		}
	})
	t.Run("tool failure records failed before its error result", func(t *testing.T) {
		root, session, transition := workspaceTransitionFixture(t, "lead-tool-failed", "call-tool-failed", "enter_worktree", "enter")
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatal(err)
		}
		transition.Status, transition.Error, transition.UpdatedAt = WorkspaceToolTransitionFailed, "workspace create failed", time.Now().UTC()
		if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
			t.Fatalf("in-call failure fact rejected before tool result: %v", err)
		}
		if _, err := Append(root, session.ID, EventToolResult, ToolResult{CallID: transition.CallID, Error: "workspace create failed"}); err != nil {
			t.Fatal(err)
		}
		appendWorkspaceRunTerminal(t, root, session, transition.RunID)
		if _, err := Replay(root, session.ID); err != nil {
			t.Fatalf("valid failed-call history failed replay: %v", err)
		}
	})
}

func TestReplayRejectsMalformedWorkspaceTransitionEvidence(t *testing.T) {
	root, session, transition := workspaceTransitionFixture(t, "lead-replay", "call-replay", "exit_worktree", "exit")
	if _, err := Append(root, session.ID, EventWorkspaceToolTransition, transition); err != nil {
		t.Fatal(err)
	}
	path, err := SessionPath(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	transition.Status, transition.UpdatedAt = WorkspaceToolTransitionApplied, time.Now().UTC()
	event := Event{SchemaVersion: SchemaVersion, SessionID: session.ID, Seq: 5, At: time.Now().UTC(), Type: EventWorkspaceToolTransition, Data: transition}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(root, session.ID); err == nil || !strings.Contains(err.Error(), "successful tool result and run terminal") {
		t.Fatalf("malformed applied transition replay error=%v", err)
	}
}

func testWorkspaceTransition(t *testing.T, sessionID, runID, callID, action string) WorkspaceToolTransition {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	transition := WorkspaceToolTransition{ID: id, SessionID: sessionID, RunID: runID, CallID: callID, WorkKind: "session", Action: action, Status: WorkspaceToolTransitionPending, CreatedAt: now, UpdatedAt: now}
	if action == "enter" || action == "export" {
		transition.WorkspaceID, err = NewID()
		if err != nil {
			t.Fatal(err)
		}
	}
	return transition
}
