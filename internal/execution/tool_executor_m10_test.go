package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

type m10HookProbe struct{ calls int }

func (h *m10HookProbe) PreToolUse(string, string, map[string]any) (bool, string, string) {
	h.calls++
	return false, "", ""
}
func (h *m10HookProbe) PostToolUse(string, string, map[string]any, string) { h.calls++ }
func (h *m10HookProbe) PreToolUseRun(_ context.Context, _ agent.ParentRun, _ string, _ string, _ map[string]any) (bool, string, string) {
	h.calls++
	return false, "", ""
}
func (h *m10HookProbe) PostToolUseRun(_ context.Context, _ agent.ParentRun, _ string, _ string, _ map[string]any, _ string) {
	h.calls++
}

func TestReadOnlyExecutorRejectsBeforeHooks(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	candidate := filepath.Join(root, "candidate")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{RunID: "run-1", SessionID: "session-1", AllowedRoot: project, CandidateRoot: candidate, FormalRoot: project, Mode: permission.ModeDefault, ReadOnly: true}
	bounds, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{RunID: authority.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID}, PermissionBounds: bounds}
	hook := &m10HookProbe{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now}, WithHookRunner(hook))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "ask_user", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied {
		t.Fatalf("outcome = %+v", outcome)
	}
	if hook.calls != 0 {
		t.Fatalf("read-only denial triggered %d hook calls", hook.calls)
	}
}
