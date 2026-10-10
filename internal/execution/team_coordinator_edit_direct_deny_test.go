package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

func TestCoordinatorDirectEditFileDeniedBeforeFileOrExecutionSideEffects(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "protected.txt")
	if err := os.WriteFile(file, []byte("original content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := m06Authority(t, root, permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, HookRunner: hooks, Now: time.Now})
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	outcome, execErr := executor.Execute(context.Background(), llm.ToolUse{
		ID:        "forged-edit-file",
		Name:      "edit_file",
		Arguments: []byte(`{"file_path":"protected.txt","old_string":"original content","new_string":"forged content"}`),
	})
	if execErr != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
		t.Fatalf("coordinator direct edit_file = %+v, %v; want static coordinator denial", outcome, execErr)
	}
	if gate.calls != 0 || hooks.preCalls != 0 || hooks.postCalls != 0 {
		t.Fatalf("denied edit_file reached permission gate/hooks: gate=%d hooks=%d/%d", gate.calls, hooks.preCalls, hooks.postCalls)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original content\n" {
		t.Fatalf("denied edit_file changed protected file: %q", got)
	}
}
