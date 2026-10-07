package conversation

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
)

type capacitySignalSubmitter struct{ capacity chan struct{} }

func (s capacitySignalSubmitter) SubmitTaskCommitted(context.Context, agent.ParentRun, agent.DelegationTask, func(agent.TaskAdmission) error) (*agent.TaskHandle, error) {
	return nil, errors.New("unexpected submit")
}

func (s capacitySignalSubmitter) CapacityChanged() <-chan struct{} { return s.capacity }

func TestTeamCapacityQueueIsBoundedFIFOAndStopsAtFullHead(t *testing.T) {
	scheduler := &teamScheduler{wake: make(chan struct{}, 1)}
	var order []int
	full := true
	if err := scheduler.enqueueCapacityResume(func() error {
		order = append(order, 1)
		if full {
			return agent.ErrDelegationQueueFull
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.enqueueCapacityResume(func() error { order = append(order, 2); return nil }); err != nil {
		t.Fatal(err)
	}
	scheduler.drainCapacityQueue()
	if !reflect.DeepEqual(order, []int{1}) || len(scheduler.waiting) != 2 {
		t.Fatalf("full head was bypassed: order=%v waiting=%d", order, len(scheduler.waiting))
	}
	full = false
	scheduler.drainCapacityQueue()
	if !reflect.DeepEqual(order, []int{1, 1, 2}) || len(scheduler.waiting) != 0 {
		t.Fatalf("FIFO queue did not drain: order=%v waiting=%d", order, len(scheduler.waiting))
	}
}

func TestTeamCapacityQueueHasHardBound(t *testing.T) {
	scheduler := &teamScheduler{wake: make(chan struct{}, 1)}
	for i := 0; i < 32; i++ {
		if err := scheduler.enqueueCapacityResume(func() error { return nil }); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := scheduler.enqueueCapacityResume(func() error { return nil }); !errors.Is(err, agent.ErrDelegationQueueFull) {
		t.Fatalf("33rd waiting resume error=%v", err)
	}
}

func TestTeamSchedulerCloseStopsCapacityWatcher(t *testing.T) {
	scheduler := &teamScheduler{submitter: capacitySignalSubmitter{capacity: make(chan struct{})}, wake: make(chan struct{}, 1), done: make(chan struct{}), active: map[string]context.CancelFunc{}, roles: map[string]agentcatalog.Definition{}}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		scheduler.capacityLoop(context.Background())
	}()
	scheduler.close()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("scheduler close left its capacity watcher running")
	}
	scheduler.close()
}
