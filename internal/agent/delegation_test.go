package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/llm"
)

type delegationProvider struct{}

func (delegationProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	return make(chan llm.Event), make(chan error)
}

type childRunnerFunc func(context.Context, ChildRunInput) ChildRunResult

func (f childRunnerFunc) Run(ctx context.Context, input ChildRunInput) ChildRunResult {
	return f(ctx, input)
}

type eventCollector struct {
	mu     sync.Mutex
	events []DelegationEvent
}

func (c *eventCollector) Publish(_ string, event DelegationEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return nil
}

func (c *eventCollector) snapshot() []DelegationEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DelegationEvent(nil), c.events...)
}

func validParent() ParentRun {
	bounds, _ := json.Marshal(map[string]any{"run_id": "parent-run", "session_id": "session", "allowed_root": "/project", "candidate_root": "/candidate"})
	return ParentRun{
		RunID: "parent-run", Work: WorkRef{Kind: WorkSession, SessionID: "session"},
		ProjectRoot: "/project", PermissionBounds: bounds, Provider: delegationProvider{}, ProviderName: "test", Model: "fake",
	}
}

func tasks(n int) []DelegationTask {
	result := make([]DelegationTask, n)
	for i := range result {
		result[i] = DelegationTask{ID: string(rune('a' + i)), Name: "task", Instruction: "inspect"}
	}
	return result
}

func TestDelegationContractValidation(t *testing.T) {
	if err := validateDelegationBatch(validParent(), tasks(1), 1<<10); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		parent ParentRun
		tasks  []DelegationTask
		max    int
	}{
		"wrong work kind": {func() ParentRun { p := validParent(); p.Work.Kind = WorkGoal; return p }(), tasks(1), 1 << 10},
		"duplicate id":    {validParent(), []DelegationTask{{ID: "a", Name: "a", Instruction: "x"}, {ID: "a", Name: "b", Instruction: "y"}}, 1 << 10},
		"empty task":      {validParent(), []DelegationTask{{ID: "a", Name: " ", Instruction: "x"}}, 1 << 10},
		"input too large": {validParent(), []DelegationTask{{ID: "a", Name: "a", Instruction: string(make([]byte, 100))}}, 10},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDelegationBatch(tc.parent, tc.tasks, tc.max); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	limits := DefaultDelegationLimits()
	if limits.Workers != 3 || limits.QueueCapacity != 32 || limits.MaxToolRounds != 8 || limits.MaxDuration != 3*time.Minute || limits.MaxSummaryBytes != 8<<10 {
		t.Fatalf("unexpected defaults: %+v", limits)
	}
}

func TestPoolDelegatorBoundsConcurrencyAndReturnsPartialResults(t *testing.T) {
	var active, peak atomic.Int32
	runner := childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		active.Add(-1)
		if input.Task.ID == "b" {
			return ChildRunResult{Status: DelegationFailed, Error: "read failed"}
		}
		return ChildRunResult{Status: DelegationSucceeded, Summary: "found"}
	})
	limits := DefaultDelegationLimits()
	limits.Workers = 2
	limits.QueueCapacity = 1
	collector := &eventCollector{}
	d, err := NewPoolDelegator(limits, runner, collector)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.RunBatch(context.Background(), validParent(), tasks(4))
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency=%d, want <=2", peak.Load())
	}
	if len(got) != 4 || got[0].TaskID != "a" || got[1].TaskID != "b" || got[2].TaskID != "c" || got[3].TaskID != "d" {
		t.Fatalf("results not in submission order: %+v", got)
	}
	if got[0].Status != DelegationSucceeded || got[1].Status != DelegationFailed || got[1].Error != "read failed" || got[2].Status != DelegationSucceeded {
		t.Fatalf("partial results lost or misreported: %+v", got)
	}
	events := collector.snapshot()
	if len(events) != 12 {
		t.Fatalf("got %d lifecycle events, want queued/running/terminal for each task: %+v", len(events), events)
	}
}

func TestPoolDelegatorClampsChildBudgetToParentDeadline(t *testing.T) {
	var got time.Duration
	runner := childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		got = input.Budget.MaxDuration
		return ChildRunResult{Status: DelegationSucceeded}
	})
	limits := DefaultDelegationLimits()
	limits.Workers = 1
	d, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parent := validParent()
	parent.Deadline = time.Now().Add(2 * time.Second)
	if _, err := d.RunBatch(context.Background(), parent, tasks(1)); err != nil {
		t.Fatal(err)
	}
	if got <= 0 || got > 2*time.Second {
		t.Fatalf("child duration budget=%s, want positive and <= 2s", got)
	}
}

func TestPoolDelegatorBackpressurePreservesFIFO(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{}, 3)
	runner := childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		started <- input.Task.ID
		<-release
		return ChildRunResult{Status: DelegationSucceeded}
	})
	limits := DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	collector := &eventCollector{}
	d, err := NewPoolDelegator(limits, runner, collector)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	done := make(chan []DelegationResult, 1)
	go func() {
		results, _ := d.RunBatch(context.Background(), validParent(), tasks(3))
		done <- results
	}()
	if id := <-started; id != "a" {
		t.Fatalf("first child=%q, want a", id)
	}
	deadline := time.After(time.Second)
	for {
		queued := 0
		for _, event := range collector.snapshot() {
			if event.Status == DelegationQueued {
				queued++
			}
		}
		if queued == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("batch did not submit all queued lifecycle events; queued=%d", queued)
		case <-time.After(time.Millisecond):
		}
	}
	release <- struct{}{}
	if id := <-started; id != "b" {
		t.Fatalf("second child=%q, want b", id)
	}
	release <- struct{}{}
	if id := <-started; id != "c" {
		t.Fatalf("third child=%q, want c", id)
	}
	release <- struct{}{}
	select {
	case results := <-done:
		if len(results) != 3 || results[2].Status != DelegationSucceeded {
			t.Fatalf("batch results=%+v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("batch did not finish after releasing workers")
	}
}

func TestPoolDelegatorIsSharedAcrossParentRuns(t *testing.T) {
	var active, peak atomic.Int32
	runner := childRunnerFunc(func(_ context.Context, _ ChildRunInput) ChildRunResult {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return ChildRunResult{Status: DelegationSucceeded}
	})
	limits := DefaultDelegationLimits()
	limits.Workers = 2
	limits.QueueCapacity = 4
	d, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var wg sync.WaitGroup
	for _, runID := range []string{"parent-1", "parent-2"} {
		parent := validParent()
		parent.RunID = runID
		parent.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": runID, "session_id": "session", "allowed_root": "/project"})
		wg.Add(1)
		go func(parent ParentRun) {
			defer wg.Done()
			if _, runErr := d.RunBatch(context.Background(), parent, tasks(3)); runErr != nil {
				t.Errorf("run batch: %v", runErr)
			}
		}(parent)
	}
	wg.Wait()
	if got := peak.Load(); got > 2 || got < 2 {
		t.Fatalf("shared pool peak concurrency=%d, want exactly 2", got)
	}
}

func TestPoolDelegatorCancelOneParentLetsOtherQueuedRunContinue(t *testing.T) {
	startedParentOne := make(chan struct{}, 1)
	runner := childRunnerFunc(func(ctx context.Context, input ChildRunInput) ChildRunResult {
		if input.ParentRunID == "parent-cancel" {
			select {
			case startedParentOne <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ChildRunResult{Status: DelegationCanceled, Error: ctx.Err().Error()}
		}
		return ChildRunResult{Status: DelegationSucceeded, Summary: "other parent completed"}
	})
	limits := DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 2
	d, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parentOne := validParent()
	parentOne.RunID = "parent-cancel"
	parentOne.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": parentOne.RunID, "session_id": "session", "allowed_root": "/project"})
	ctxOne, cancelOne := context.WithCancel(context.Background())
	firstDone := make(chan []DelegationResult, 1)
	go func() {
		results, _ := d.RunBatch(ctxOne, parentOne, tasks(2))
		firstDone <- results
	}()
	select {
	case <-startedParentOne:
	case <-time.After(time.Second):
		t.Fatal("first parent did not start")
	}
	parentTwo := validParent()
	parentTwo.RunID = "parent-continue"
	parentTwo.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": parentTwo.RunID, "session_id": "session", "allowed_root": "/project"})
	secondDone := make(chan []DelegationResult, 1)
	go func() {
		results, _ := d.RunBatch(context.Background(), parentTwo, tasks(2))
		secondDone <- results
	}()
	time.Sleep(10 * time.Millisecond) // allow the second batch to fill the shared queue
	cancelOne()
	select {
	case results := <-firstDone:
		if len(results) != 2 || results[0].Status != DelegationCanceled || results[1].Status != DelegationCanceled {
			t.Fatalf("canceled parent results=%+v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled parent batch did not settle")
	}
	select {
	case results := <-secondDone:
		for _, result := range results {
			if result.Status != DelegationSucceeded {
				t.Fatalf("other parent queue did not continue: %+v", results)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("other parent's queued work did not continue")
	}
}

func TestPoolDelegatorCancelsQueuedAndRunningWork(t *testing.T) {
	started := make(chan struct{}, 1)
	runner := childRunnerFunc(func(ctx context.Context, _ ChildRunInput) ChildRunResult {
		started <- struct{}{}
		<-ctx.Done()
		return ChildRunResult{Status: DelegationCanceled, Error: ctx.Err().Error()}
	})
	limits := DefaultDelegationLimits()
	limits.Workers = 1
	limits.QueueCapacity = 1
	d, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []DelegationResult, 1)
	go func() {
		result, _ := d.RunBatch(ctx, validParent(), tasks(4))
		done <- result
	}()
	<-started
	cancel()
	select {
	case results := <-done:
		if len(results) != 4 {
			t.Fatalf("got %d results", len(results))
		}
		for _, result := range results {
			if result.Status != DelegationCanceled {
				t.Fatalf("task %s status=%s, want canceled", result.TaskID, result.Status)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("canceled batch did not finish")
	}
}

func TestPoolDelegatorRejectsOversizeAndCanceledInput(t *testing.T) {
	d, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 1, MaxInputBytes: 8}, childRunnerFunc(func(context.Context, ChildRunInput) ChildRunResult {
		return ChildRunResult{Status: DelegationSucceeded}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.RunBatch(context.Background(), validParent(), tasks(2)); err == nil {
		t.Fatal("expected oversized batch error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.RunBatch(ctx, validParent(), tasks(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context canceled", err)
	}
}
