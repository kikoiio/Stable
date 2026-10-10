package execution

import (
	"context"
	"encoding/json"
	"testing"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/workspace"
)

type workspaceLifecycleSequenceGate struct {
	decision permission.PermissionDecision
	sequence *[]string
}

func (g workspaceLifecycleSequenceGate) Authorize(_ context.Context, _ permission.Authority, _ permission.Operation) (permission.PermissionDecision, error) {
	*g.sequence = append(*g.sequence, "gate")
	return g.decision, nil
}

func TestWorkspaceLifecycleToolRequiresExplicitPermissionBeforeHost(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeBypass, "")
	host := NewWorkspaceLifecycleToolHost()
	hostCalls := 0
	host.Bind(func(_ context.Context, request agent.ExecutionRequest, _ *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
		hostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded, Content: request.RunID}, nil
	})
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "explicit deny"}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate}, WithWorkspaceLifecycleToolHost(host))
	runner, err := factory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "lifecycle-denied", Name: "enter_worktree", Arguments: json.RawMessage(`{"label":"next"}`)})
	if err != nil || outcome.Status != agent.ToolDenied || !outcome.IsError {
		t.Fatalf("denied lifecycle outcome=%+v err=%v", outcome, err)
	}
	if gate.calls != 1 || gate.seen.Kind != permission.OpWorkspaceLifecycle || gate.seen.Name != "enter_worktree" || gate.seen.Target != "" {
		t.Fatalf("lifecycle permission operation=%+v calls=%d", gate.seen, gate.calls)
	}
	if hostCalls != 0 {
		t.Fatalf("denied lifecycle call reached trusted host %d times", hostCalls)
	}
	if got := (permission.Policy{}).Decide(authority, permission.Operation{ID: "p", Kind: permission.OpWorkspaceLifecycle, Name: "enter_worktree"}); got.Kind != permission.DecisionAsk {
		t.Fatalf("bypass mode must still ask for lifecycle operations: %+v", got)
	}
}

func TestWorkspaceLifecycleToolsAreAbsentFromChildSurface(t *testing.T) {
	host := NewWorkspaceLifecycleToolHost()
	hostCalls := 0
	host.Bind(func(_ context.Context, request agent.ExecutionRequest, _ *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
		hostCalls++
		return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded, Content: request.RunID}, nil
	})
	base := NewToolExecutorFactory(ToolExecutorDeps{}, WithWorkspaceLifecycleToolHost(host))
	childFactory := ReadOnlyExecutorFactory(base)
	authority := m06Authority(t, t.TempDir(), permission.ModeDefault, "")
	runner, err := childFactory.ForRun(m06Request(t, authority))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "child-lifecycle", Name: "exit_worktree", Arguments: json.RawMessage(`{}`)})
	if err != nil || outcome.Status == agent.ToolSucceeded || !outcome.IsError {
		t.Fatalf("child lifecycle call outcome=%+v err=%v", outcome, err)
	}
	if hostCalls != 0 {
		t.Fatalf("child lifecycle call reached host %d times", hostCalls)
	}
	for _, schema := range ReadOnlyToolSchemas() {
		if isWorkspaceLifecycleTool(schema.Name) {
			t.Fatalf("child schema exposed lifecycle tool %q", schema.Name)
		}
	}
}

func TestWorkspaceLifecyclePermissionPrecedesPreToolHook(t *testing.T) {
	for _, tc := range []struct {
		name         string
		decision     permission.PermissionDecision
		wantSequence []string
		wantPre      int
		wantHost     int
		hookDenies   bool
	}{
		{name: "deny", decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "denied"}, wantSequence: []string{"gate"}},
		{name: "ask without approval", decision: permission.PermissionDecision{Kind: permission.DecisionAsk}, wantSequence: []string{"gate"}},
		{name: "allow then hook deny", decision: permission.PermissionDecision{Kind: permission.DecisionAllow}, wantSequence: []string{"gate", "pre"}, wantPre: 1, hookDenies: true},
		{name: "allow", decision: permission.PermissionDecision{Kind: permission.DecisionAllow}, wantSequence: []string{"gate", "pre", "host", "post"}, wantPre: 1, wantHost: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequence := []string{}
			gate := workspaceLifecycleSequenceGate{decision: tc.decision, sequence: &sequence}
			hooks := &executorTestHookRunner{sequence: &sequence, preRejected: tc.hookDenies}
			host := NewWorkspaceLifecycleToolHost()
			hostCalls := 0
			host.Bind(func(_ context.Context, request agent.ExecutionRequest, _ *workspace.WriterLease, call llm.ToolUse) (agent.ToolOutcome, error) {
				hostCalls++
				sequence = append(sequence, "host")
				return agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolSucceeded, Content: request.RunID}, nil
			})
			formal := t.TempDir()
			authority := m06Authority(t, formal, permission.ModeDefault, "")
			factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, HookRunner: hooks}, WithWorkspaceLifecycleToolHost(host))
			runner, err := factory.ForRun(m06Request(t, authority))
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "ordered-lifecycle", Name: "exit_worktree", Arguments: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("Execute error=%v outcome=%+v", err, outcome)
			}
			if hooks.preCalls != tc.wantPre || hostCalls != tc.wantHost {
				t.Fatalf("pre calls=%d host calls=%d, want %d/%d; outcome=%+v", hooks.preCalls, hostCalls, tc.wantPre, tc.wantHost, outcome)
			}
			if tc.hookDenies && hooks.postCalls != 0 {
				t.Fatalf("denied pre-hook unexpectedly ran post hook: calls=%d", hooks.postCalls)
			}
			if len(sequence) != len(tc.wantSequence) {
				t.Fatalf("sequence=%v, want %v", sequence, tc.wantSequence)
			}
			for i := range sequence {
				if sequence[i] != tc.wantSequence[i] {
					t.Fatalf("sequence=%v, want %v", sequence, tc.wantSequence)
				}
			}
		})
	}
}
