package core

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestWorkflowTimerAndExternalSignals(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	events := []string{}
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { return 1, nil }, activity.RegisterOptions{Name: "CheckInterval"})
	env.RegisterActivityWithOptions(func(context.Context, string) (bool, error) { return false, nil }, activity.RegisterOptions{Name: "WaitingForHuman"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ string, eventID string) (bool, error) {
		events = append(events, eventID)
		return len(events) >= 4, nil
	}, activity.RegisterOptions{Name: "EvaluateGoal"})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(GoalEventSignal, "design-event") }, 100*time.Millisecond)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(GoalEventSignal, "external-failure") }, 300*time.Millisecond)
	env.ExecuteWorkflow(GoalWorkflow, "goal-1")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0] != "timer-goal-1-0" || events[1] != "design-event" || events[2] != "external-failure" || events[3] != "timer-goal-1-3" {
		t.Fatalf("unexpected evaluation order: %v", events)
	}
}

func TestWorkflowDrainsQueuedSignalBeforeCompletion(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	events := []string{}
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { return 1, nil }, activity.RegisterOptions{Name: "CheckInterval"})
	env.RegisterActivityWithOptions(func(context.Context, string) (bool, error) { return false, nil }, activity.RegisterOptions{Name: "WaitingForHuman"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ string, eventID string) (bool, error) {
		events = append(events, eventID)
		if len(events) == 1 {
			env.SignalWorkflow(GoalEventSignal, "queued-before-completion")
		}
		return true, nil
	}, activity.RegisterOptions{Name: "EvaluateGoal"})
	env.ExecuteWorkflow(GoalWorkflow, "goal-2")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1] != "queued-before-completion" {
		t.Fatalf("queued signal was not evaluated: %v", events)
	}
}

func TestWorkflowSuppressesTimerWhileWaitingForHuman(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	events := []string{}
	waiting := true
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { return 1, nil }, activity.RegisterOptions{Name: "CheckInterval"})
	env.RegisterActivityWithOptions(func(context.Context, string) (bool, error) { return waiting, nil }, activity.RegisterOptions{Name: "WaitingForHuman"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ string, eventID string) (bool, error) {
		events = append(events, eventID)
		if len(events) == 2 {
			waiting = false
		}
		return len(events) >= 3, nil
	}, activity.RegisterOptions{Name: "EvaluateGoal"})
	// Well past several timer intervals: while the question is unanswered, the
	// timer must not trigger another evaluation.
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(GoalEventSignal, "human-reply") }, 10*time.Second)
	env.ExecuteWorkflow(GoalWorkflow, "goal-3")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1] != "human-reply" || events[2] != "timer-goal-3-2" {
		t.Fatalf("timer fired while waiting for human, or wrong order: %v", events)
	}
}
