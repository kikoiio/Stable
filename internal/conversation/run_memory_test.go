package conversation

import (
	"context"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/memory"
	"stable/internal/sessionlog"
)

type captureMemoryManager struct {
	context    memory.RunMemoryContext
	completion memory.RunCompletion
	calls      int
}

func (m *captureMemoryManager) PrepareRun(context.Context, string, string, string, string) (memory.RunMemoryContext, error) {
	return m.context, nil
}
func (*captureMemoryManager) List(context.Context, string) ([]memory.MemoryHeader, error) {
	return nil, nil
}
func (*captureMemoryManager) Read(context.Context, string, memory.MemoryScope, string) (memory.MemoryEntry, error) {
	return memory.MemoryEntry{}, nil
}
func (*captureMemoryManager) Save(context.Context, string, memory.MemoryChange) error { return nil }
func (*captureMemoryManager) Delete(context.Context, string, memory.MemoryScope, string) error {
	return nil
}
func (*captureMemoryManager) Clear(context.Context, string, memory.MemoryScope) (int, error) {
	return 0, nil
}
func (m *captureMemoryManager) CompleteRun(_ context.Context, completion memory.RunCompletion) {
	m.calls++
	m.completion = completion
}
func (*captureMemoryManager) MaybeConsolidate(context.Context, string) error { return nil }

func TestRenderMemoryContextIsBoundedAndContainsSelectedText(t *testing.T) {
	got := renderMemoryContext(memory.RunMemoryContext{
		InstructionText: "Follow local guidance.",
		UserIndex:       "Prefer concise replies.",
		Selected:        []memory.MemoryEntry{{MemoryHeader: memory.MemoryHeader{Scope: memory.ScopeProject, Name: "Build", UpdatedAt: time.Now().Add(-25 * time.Hour)}, Body: "Uses Go."}},
	})
	for _, want := range []string{"Follow local guidance.", "Prefer concise replies.", "Uses Go.", "project memory", "updated ", "older than 24 hours"} {
		if !strings.Contains(got, want) {
			t.Fatalf("memory context missing %q: %s", want, got)
		}
	}
	if len(got) > memoryContextLimit+512 {
		t.Fatalf("memory prefix exceeded cap: %d bytes", len(got))
	}
	large := renderMemoryContext(memory.RunMemoryContext{InstructionText: strings.Repeat("x", memoryContextLimit*2)})
	if len(large) > memoryContextLimit {
		t.Fatalf("large memory prefix exceeded cap: %d bytes", len(large))
	}
	largeUTF8 := renderMemoryContext(memory.RunMemoryContext{InstructionText: strings.Repeat("记忆规则", memoryContextLimit)})
	if len(largeUTF8) > memoryContextLimit {
		t.Fatalf("UTF-8 memory prefix exceeded cap: %d bytes", len(largeUTF8))
	}
	if !strings.Contains(largeUTF8, "<Stable instructions>") || !strings.HasSuffix(largeUTF8, "</Stable instructions>") {
		t.Fatal("UTF-8 truncation broke the memory context section")
	}
}

func TestCompleteMemoryRunFiltersNewSessionTextAndTracksSave(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "memory-cursor")
	if err != nil {
		t.Fatal(err)
	}
	appendMessage := func(role, kind, text string) {
		t.Helper()
		if _, appendErr := sessionlog.Append(root, session.ID, sessionlog.EventMessage, sessionlog.Message{Role: role, Kind: kind, Text: text}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	appendMessage("user", "text", "old preference")
	cursor := uint64(2)
	appendMessage("user", "text", "new preference")
	appendMessage("assistant", "text", "assistant output")
	appendMessage("user", "goal_request", "goal criteria")
	if _, err = sessionlog.Append(root, session.ID, sessionlog.EventMemoryAction, sessionlog.MemoryActionRecord{Scope: "user", Entry: "tone", Operation: "save", State: "success", RunID: "run-1", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	manager := &captureMemoryManager{}
	svc := &Service{deps: Deps{ProjectRoot: root}, memory: NewMemoryGate(manager, root)}
	svc.completeMemoryRun(agent.ExecutionRequest{RunID: "run-1", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}}, cursor)
	got := manager.completion
	if manager.calls != 1 || !got.MainAgentWroteMemory {
		t.Fatalf("completion not delivered or save not detected: calls=%d completion=%+v", manager.calls, got)
	}
	if got.ThroughSeq <= cursor || len(got.Messages) != 1 || got.Messages[0].Text != "new preference" {
		t.Fatalf("completion cursor/filter = %+v", got)
	}
}

func TestCompleteMemoryRunGoalAllowsOnlySayAndReply(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "goal-memory")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []sessionlog.Message{
		{Role: "user", Kind: "goal_say", Text: "focus the connector"},
		{Role: "user", Kind: "goal_reply", Text: "J1.2"},
		{Role: "user", Kind: "text", Text: "ordinary chat"},
		{Role: "user", Kind: "goal_request", Text: "acceptance criteria"},
		{Role: "assistant", Kind: "text", Text: "implementation result"},
		{Role: "user", Kind: "tool_result", Text: "tool output"},
	} {
		if _, err = sessionlog.Append(root, session.ID, sessionlog.EventMessage, msg); err != nil {
			t.Fatal(err)
		}
	}
	manager := &captureMemoryManager{}
	svc := &Service{deps: Deps{ProjectRoot: root}, memory: NewMemoryGate(manager, root)}
	svc.completeMemoryRun(agent.ExecutionRequest{RunID: "goal-run", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID}}, 0)
	got := manager.completion.Messages
	if len(got) != 2 || got[0].Text != "focus the connector" || got[1].Text != "J1.2" || got[0].Kind != "goal_reply" {
		t.Fatalf("goal completion included disallowed messages: %+v", got)
	}
}
