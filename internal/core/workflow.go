package core

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const GoalEventSignal = "GoalEvent"
const TaskQueue = "stable"

// GoalWorkflow stores only IDs and deterministic control flow in Temporal history.
func GoalWorkflow(ctx workflow.Context, goalID string) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 8 * time.Minute,
		HeartbeatTimeout:    15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 30 * time.Second, MaximumAttempts: 3},
	})
	var seconds int
	if err := workflow.ExecuteActivity(ctx, "CheckInterval", goalID).Get(ctx, &seconds); err != nil {
		return err
	}
	if seconds <= 0 {
		seconds = 30
	}
	events := workflow.GetSignalChannel(ctx, GoalEventSignal)
	cycle := 0
	eventID := fmt.Sprintf("timer-%s-%d", goalID, cycle)
	for {
		var done bool
		if err := workflow.ExecuteActivity(ctx, "EvaluateGoal", goalID, eventID).Get(ctx, &done); err != nil {
			return err
		}
		if done {
			if events.ReceiveAsync(&eventID) {
				cycle++
				continue
			}
			return nil
		}
		timerCtx, cancel := workflow.WithCancel(ctx)
		timer := workflow.NewTimer(timerCtx, time.Duration(seconds)*time.Second)
		next := ""
		selector := workflow.NewSelector(ctx)
		selector.AddReceive(events, func(ch workflow.ReceiveChannel, _ bool) { ch.Receive(ctx, &next) })
		selector.AddFuture(timer, func(workflow.Future) { next = fmt.Sprintf("timer-%s-%d", goalID, cycle+1) })
		selector.Select(ctx)
		cancel()
		cycle++
		eventID = next
	}
}
