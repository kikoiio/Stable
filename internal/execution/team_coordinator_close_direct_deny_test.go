package execution

import (
	"context"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
)

func TestCoordinatorDirectTeamCloseIsDeniedBeforeUserCloseHost(t *testing.T) {
	authority := m06Authority(t, t.TempDir(), permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true
	request.TeamCoordinatorTeamID = "0123456789abcdef0123456789abcdef"

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	teamHost := &agent.TeamToolHost{}
	teamHostCalls := 0
	teamHost.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		teamHostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded, Content: "closed"}, nil
	})

	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, HookRunner: hooks, Now: time.Now}, WithTeamToolHost(teamHost))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := executor.Execute(context.Background(), llm.ToolUse{
		ID:        "forged-user-close",
		Name:      "team_close",
		Arguments: []byte(`{"team_id":"0123456789abcdef0123456789abcdef"}`),
	})
	if err != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
		t.Fatalf("coordinator direct team_close outcome=%+v err=%v; want coordinator denial", outcome, err)
	}
	if teamHostCalls != 0 {
		t.Fatalf("forged coordinator team_close reached user close host %d times", teamHostCalls)
	}
	if gate.calls != 0 {
		t.Fatalf("forged coordinator team_close reached permission gate %d times", gate.calls)
	}
	if hooks.preCalls != 0 || hooks.postCalls != 0 {
		t.Fatalf("forged coordinator team_close reached hooks pre/post=%d/%d", hooks.preCalls, hooks.postCalls)
	}
}
