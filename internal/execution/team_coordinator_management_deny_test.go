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

type coordinatorManagementDelegatorProbe struct{ calls int }

func (d *coordinatorManagementDelegatorProbe) RunBatch(context.Context, agent.ParentRun, []agent.DelegationTask) ([]agent.DelegationResult, error) {
	d.calls++
	return nil, nil
}

func (d *coordinatorManagementDelegatorProbe) RunTask(context.Context, agent.ParentRun, agent.DelegationTask) (agent.DelegationResult, error) {
	d.calls++
	return agent.DelegationResult{}, nil
}

func TestCoordinatorCannotCallLeadOnlyTeamManagementBeforeSideEffects(t *testing.T) {
	authority := m06Authority(t, t.TempDir(), permission.ModeBypass, "")
	request := m06Request(t, authority)
	request.TeamCoordinator = true
	request.TeamCoordinatorTeamID = "0123456789abcdef0123456789abcdef"

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{}
	teams := &agent.TeamToolHost{}
	teamHostCalls := 0
	teams.Bind(func(_ context.Context, _ agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		teamHostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded}, nil
	})
	delegator := &coordinatorManagementDelegatorProbe{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate: gate, HookRunner: hooks, Now: time.Now,
	}, WithTeamToolHost(teams), WithDelegator(delegator, nil))
	executor, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, arguments string
	}{
		{name: "team_create", arguments: `{"name":"forged-team"}`},
		{name: "team_member_stop", arguments: `{"team_id":"0123456789abcdef0123456789abcdef","member_id":"1123456789abcdef0123456789abcdef"}`},
	} {
		outcome, execErr := executor.Execute(context.Background(), llm.ToolUse{
			ID: "forged-" + tc.name, Name: tc.name, Arguments: []byte(tc.arguments),
		})
		if execErr != nil || outcome.Status != agent.ToolDenied || !outcome.IsError || !strings.Contains(outcome.Content, "coordinator mode") {
			t.Fatalf("coordinator direct %s = %+v, %v; want coordinator denial", tc.name, outcome, execErr)
		}
	}
	if gate.calls != 0 || hooks.preCalls != 0 || hooks.postCalls != 0 || teamHostCalls != 0 || delegator.calls != 0 {
		t.Fatalf("denied coordinator management calls reached downstream effects: gate=%d hooks=%d/%d team_host=%d delegator=%d", gate.calls, hooks.preCalls, hooks.postCalls, teamHostCalls, delegator.calls)
	}
}
