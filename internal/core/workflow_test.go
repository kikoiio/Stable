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

func TestWorkflowWaitingForHumanSignalResumesImmediately(t *testing.T) {
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
	// The reply arrives well before the 5x-interval backoff (5s) would fire:
	// it must resume the goal immediately.
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(GoalEventSignal, "human-reply") }, 2*time.Second)
	env.ExecuteWorkflow(GoalWorkflow, "goal-3")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0] != "timer-goal-3-0" || events[1] != "human-reply" || events[2] != "timer-goal-3-2" {
		t.Fatalf("human reply did not pre-empt the backoff timer: %v", events)
	}
}

func TestWorkflowWaitingForHumanBackoffSelfHeals(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	events := []string{}
	waiting := true
	parkedAtThreeSeconds := false
	env.RegisterActivityWithOptions(func(context.Context, string) (int, error) { return 1, nil }, activity.RegisterOptions{Name: "CheckInterval"})
	env.RegisterActivityWithOptions(func(context.Context, string) (bool, error) { return waiting, nil }, activity.RegisterOptions{Name: "WaitingForHuman"})
	env.RegisterActivityWithOptions(func(_ context.Context, _ string, eventID string) (bool, error) {
		events = append(events, eventID)
		if len(events) == 2 {
			waiting = false
		}
		return len(events) >= 3, nil
	}, activity.RegisterOptions{Name: "EvaluateGoal"})
	// Past several fast intervals but before the 5s backoff: the question must
	// still be parked — the fast timer stays suppressed while waiting.
	env.RegisterDelayedCallback(func() { parkedAtThreeSeconds = len(events) == 1 }, 3*time.Second)
	env.ExecuteWorkflow(GoalWorkflow, "goal-4")
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !parkedAtThreeSeconds {
		t.Fatalf("fast timer fired while waiting for human: %v", events)
	}
	// The long backoff re-evaluates a transient question even with no reply.
	if len(events) != 3 || events[0] != "timer-goal-4-0" || events[1] != "timer-goal-4-1" || events[2] != "timer-goal-4-2" {
		t.Fatalf("backoff did not re-evaluate the parked goal: %v", events)
	}
}
