package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPoolDelegatorUsesDefaultWorkerAndQueueCapacity(t *testing.T) {
	limits := DefaultDelegationLimits()
	if limits.Workers != 3 || limits.QueueCapacity != 32 {
		t.Fatalf("default delegation capacity = %d workers/%d queue, want 3/32", limits.Workers, limits.QueueCapacity)
	}

	const timeout = 3 * time.Second
	started := make(chan string, limits.Workers+limits.QueueCapacity)
	finished := make(chan string, limits.Workers+limits.QueueCapacity)
	release := make(chan struct{})
	runner := childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		started <- input.Task.ID
		<-release
		finished <- input.Task.ID
		return ChildRunResult{Status: DelegationSucceeded}
	})
	pool, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce bool
	defer func() {
		if !releaseOnce {
			close(release)
		}
		pool.Close()
	}()

	parent := validParent()
	handles := make([]*TaskHandle, 0, limits.Workers+limits.QueueCapacity)
	submit := func(index int) {
		t.Helper()
		taskID := fmt.Sprintf("default-capacity-%02d", index)
		handle, submitErr := pool.SubmitTask(context.Background(), parent, DelegationTask{
			ID: taskID, Name: taskID, Instruction: "exercise the default delegation capacity",
		})
		if submitErr != nil {
			t.Fatalf("submit task %s: %v", taskID, submitErr)
		}
		handles = append(handles, handle)
	}

	for i := 0; i < limits.Workers; i++ {
		submit(i)
	}
	active := make(map[string]bool, limits.Workers)
	for len(active) < limits.Workers {
		select {
		case taskID := <-started:
			active[taskID] = true
		case <-time.After(timeout):
			t.Fatalf("only %d of %d default workers started", len(active), limits.Workers)
		}
	}

	for i := limits.Workers; i < limits.Workers+limits.QueueCapacity; i++ {
		submit(i)
	}
	if len(started) != 0 {
		t.Fatalf("queued work started while all workers were gated: %d unexpected starts", len(started))
	}
	_, err = pool.SubmitTask(context.Background(), parent, DelegationTask{
		ID: "default-capacity-overflow", Name: "overflow", Instruction: "must be rejected without a worker slot",
	})
	if !errors.Is(err, ErrDelegationQueueFull) {
		t.Fatalf("33rd pending task error = %v, want ErrDelegationQueueFull", err)
	}

	close(release)
	releaseOnce = true
	for _, handle := range handles {
		select {
		case result := <-handle.Results:
			if result.Status != DelegationSucceeded {
				t.Fatalf("task %s status = %s, want succeeded", result.TaskID, result.Status)
			}
		case <-time.After(timeout):
			t.Fatalf("task %s did not settle after releasing workers", handle.TaskID)
		}
	}

	seen := make(map[string]bool, len(handles))
	for range handles {
		select {
		case taskID := <-finished:
			if seen[taskID] {
				t.Fatalf("task %s ran more than once", taskID)
			}
			seen[taskID] = true
		case <-time.After(timeout):
			t.Fatalf("only %d of %d accepted tasks ran", len(seen), len(handles))
		}
	}
	if len(seen) != limits.Workers+limits.QueueCapacity {
		t.Fatalf("executed %d tasks, want %d", len(seen), limits.Workers+limits.QueueCapacity)
	}
	pool.Close()
}
