package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type admissionReporter struct {
	committed *atomic.Bool
	queued    atomic.Bool
	err       error
}

func (r *admissionReporter) Publish(_ string, event DelegationEvent) error {
	if event.Status == DelegationQueued {
		if !r.committed.Load() {
			return errors.New("queued published before team admission commit")
		}
		if r.err != nil {
			return r.err
		}
		r.queued.Store(true)
	}
	return nil
}

func TestCommittedAdmissionDoesNotExposeProviderBeforeDurableFacts(t *testing.T) {
	var committed atomic.Bool
	reporter := &admissionReporter{committed: &committed}
	var calls atomic.Int32
	runner := childRunnerFunc(func(_ context.Context, _ ChildRunInput) ChildRunResult {
		calls.Add(1)
		if !committed.Load() || !reporter.queued.Load() {
			return ChildRunResult{Status: DelegationFailed, Error: "provider started before admission facts"}
		}
		return ChildRunResult{Status: DelegationSucceeded}
	})
	d, err := NewPoolDelegator(DefaultDelegationLimits(), runner, reporter)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan *TaskHandle, 1)
	errorsCh := make(chan error, 1)
	go func() {
		handle, err := d.SubmitTaskCommitted(context.Background(), validParent(), tasks(1)[0], func(admission TaskAdmission) error {
			if admission.TaskID != "a" || admission.BatchID == "" {
				return errors.New("missing tentative identity")
			}
			close(entered)
			<-release
			committed.Store(true)
			return nil
		})
		result <- handle
		errorsCh <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("commit gate did not run")
	}
	if calls.Load() != 0 || len(d.queue) != 0 || reporter.queued.Load() {
		t.Fatal("tentative reservation became worker-visible")
	}
	close(release)
	handle := <-result
	if err := <-errorsCh; err != nil {
		t.Fatal(err)
	}
	if outcome := awaitSubmittedResult(t, handle); outcome.Status != DelegationSucceeded || calls.Load() != 1 {
		t.Fatalf("committed outcome=%+v", outcome)
	}
}

func TestCommittedAdmissionFailureReleasesReservationWithoutCallingProvider(t *testing.T) {
	for _, failure := range []string{"callback", "queued", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			var committed atomic.Bool
			reporter := &admissionReporter{committed: &committed}
			if failure == "queued" {
				reporter.err = errors.New("disk write failure")
			}
			var calls atomic.Int32
			d, err := NewPoolDelegator(DefaultDelegationLimits(), childRunnerFunc(func(context.Context, ChildRunInput) ChildRunResult {
				calls.Add(1)
				return ChildRunResult{Status: DelegationSucceeded}
			}), reporter)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handle, err := d.SubmitTaskCommitted(ctx, validParent(), tasks(1)[0], func(TaskAdmission) error {
				if failure == "callback" {
					return errors.New("team accepted fact write failed")
				}
				committed.Store(true)
				if failure == "canceled" {
					cancel()
				}
				return nil
			})
			if err == nil || handle != nil || calls.Load() != 0 || len(d.queue) != 0 || len(d.queueSlots) != 0 {
				t.Fatalf("failure=%s exposed work/kept reservation: handle=%+v err=%v calls=%d", failure, handle, err, calls.Load())
			}
			select {
			case <-d.CapacityChanged():
			case <-time.After(time.Second):
				t.Fatal("failed reservation did not wake waiting capacity")
			}
		})
	}
}

func TestCommittedAdmissionQueueFullDoesNotCommitAndCapacityWakeAllowsRetry(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	limits := DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	d, err := NewPoolDelegator(limits, childRunnerFunc(func(ctx context.Context, input ChildRunInput) ChildRunResult {
		if input.Task.ID == "first" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ChildRunResult{Status: DelegationSucceeded}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.SubmitTask(context.Background(), validParent(), DelegationTask{ID: "first", Name: "first", Instruction: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case <-d.CapacityChanged():
	default:
	}
	second, err := d.SubmitTask(context.Background(), validParent(), DelegationTask{ID: "second", Name: "second", Instruction: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	commit := func(TaskAdmission) error { commits.Add(1); return nil }
	thirdTask := DelegationTask{ID: "third", Name: "third", Instruction: "inspect"}
	if handle, err := d.SubmitTaskCommitted(context.Background(), validParent(), thirdTask, commit); !errors.Is(err, ErrDelegationQueueFull) || handle != nil || commits.Load() != 0 {
		t.Fatalf("full queue committed admission: %v %v", handle, err)
	}
	close(release)
	select {
	case <-d.CapacityChanged():
	case <-time.After(time.Second):
		t.Fatal("dequeue did not notify scheduler")
	}
	third, err := d.SubmitTaskCommitted(context.Background(), validParent(), thirdTask, commit)
	if err != nil {
		t.Fatal(err)
	}
	if commits.Load() != 1 {
		t.Fatal("retry commit was not called exactly once")
	}
	for _, handle := range []*TaskHandle{first, second, third} {
		if result := awaitSubmittedResult(t, handle); result.Status != DelegationSucceeded {
			t.Fatal(result)
		}
	}
	for i := 0; i < 100; i++ {
		d.notifyCapacity()
	}
	if len(d.capacityChanged) != 1 {
		t.Fatal("capacity notification is not coalesced")
	}
}

func TestClosingPoolWhileCommitRunsNeverStartsProvider(t *testing.T) {
	var calls atomic.Int32
	d, err := NewPoolDelegator(DefaultDelegationLimits(), childRunnerFunc(func(context.Context, ChildRunInput) ChildRunResult {
		calls.Add(1)
		return ChildRunResult{Status: DelegationSucceeded}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	submission := make(chan error, 1)
	go func() {
		_, err := d.SubmitTaskCommitted(context.Background(), validParent(), tasks(1)[0], func(TaskAdmission) error { close(entered); <-release; return nil })
		submission <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() { d.Close(); close(closed) }()
	select {
	case <-d.life.Done():
	case <-time.After(time.Second):
		t.Fatal("pool did not begin closing")
	}
	close(release)
	if err := <-submission; !errors.Is(err, ErrDelegationClosed) {
		t.Fatalf("closing submission=%v", err)
	}
	<-closed
	if calls.Load() != 0 || len(d.queueSlots) != 0 {
		t.Fatal("shutdown admitted tentative work")
	}
}
