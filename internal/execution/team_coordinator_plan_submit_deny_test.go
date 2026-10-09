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

func TestCoordinatorCannotSubmitTeamPlanDirectlyBeforeSideEffects(t *testing.T) {
	authority := m06Authority(t, t.TempDir(), permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	teamTools := &agent.TeamToolHost{}
	teamToolCalls := 0
	teamTools.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		teamToolCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded}, nil
	})

	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate: gate, HookRunner: hooks, Now: time.Now,
	}, WithTeamToolHost(teamTools))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.Execute(context.Background(), llm.ToolUse{
		ID: "forged-plan-submit", Name: "team_plan_submit",
		Arguments: []byte(`{"team_id":"team-1","body":"approved plan"}`),
	})
	if err != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
		t.Fatalf("coordinator direct team_plan_submit = %+v, %v; want static denial", outcome, err)
	}
	if gate.calls != 0 || hooks.preCalls != 0 || hooks.postCalls != 0 || teamToolCalls != 0 {
		t.Fatalf("denied coordinator call reached downstream effects: gate=%d hooks=%d/%d team_tool=%d", gate.calls, hooks.preCalls, hooks.postCalls, teamToolCalls)
	}
}
