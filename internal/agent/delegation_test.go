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
	for _, event := range events {
		if event.SessionID != "session" {
			t.Fatalf("delegation event lost its session scope: %+v", event)
		}
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

func TestPoolDelegatorRunTaskSupportsGoalWithoutChangingBatchContract(t *testing.T) {
	var seen WorkRef
	runner := childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		seen = input.Work
		return ChildRunResult{Status: DelegationSucceeded, Summary: "goal inspected"}
	})
	d, err := NewPoolDelegator(DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	parent := validParent()
	parent.Work = WorkRef{Kind: WorkGoal, SessionID: "session", GoalID: "goal-1", WorkItemID: "item-2"}
	result, err := d.RunTask(context.Background(), parent, DelegationTask{ID: "hook-1", Name: "inspect hook", Instruction: "inspect the goal"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != DelegationSucceeded || result.Summary != "goal inspected" || seen != parent.Work {
		t.Fatalf("result=%+v work=%+v, want successful goal child", result, seen)
	}
	if _, err = d.RunBatch(context.Background(), parent, tasks(1)); err == nil {
		t.Fatal("RunBatch accepted Goal work; the batch contract must remain Session-only")
	}
}

func TestPoolDelegatorRunTaskRejectsFullQueue(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runner := childRunnerFunc(func(ctx context.Context, _ ChildRunInput) ChildRunResult {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
			return ChildRunResult{Status: DelegationSucceeded}
		case <-ctx.Done():
			return ChildRunResult{Status: DelegationCanceled, Error: ctx.Err().Error()}
		}
	})
	limits := DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	d, err := NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parent := validParent()
	firstDone := make(chan struct{})
	go func() {
		_, _ = d.RunTask(context.Background(), parent, DelegationTask{ID: "one", Name: "one", Instruction: "one"})
		close(firstDone)
	}()
	<-started
	queuedDone := make(chan struct{})
	go func() {
		_, _ = d.RunTask(context.Background(), parent, DelegationTask{ID: "two", Name: "two", Instruction: "two"})
		close(queuedDone)
	}()
	deadline := time.After(time.Second)
	for len(d.queue) != 1 {
		select {
		case <-deadline:
			t.Fatal("second task did not enter bounded queue")
		case <-time.After(time.Millisecond):
		}
	}
	result, err := d.RunTask(context.Background(), parent, DelegationTask{ID: "three", Name: "three", Instruction: "three"})
	if err != nil || result.Status != DelegationFailed || result.Error != "delegation queue is full" {
		t.Fatalf("full queue result=%+v err=%v", result, err)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first task did not finish")
	}
	select {
	case <-queuedDone:
	case <-time.After(time.Second):
		t.Fatal("queued task did not finish")
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

type delegationReporterFunc func(string, DelegationEvent) error

func (f delegationReporterFunc) Publish(runID string, event DelegationEvent) error {
	return f(runID, event)
}

func awaitSubmittedResult(t *testing.T, handle *TaskHandle) DelegationResult {
	t.Helper()
	select {
	case result, ok := <-handle.Results:
		if !ok {
			t.Fatal("task closed without a result")
		}
		return result
	case <-time.After(time.Second):
		t.Fatal("accepted task did not settle")
		return DelegationResult{}
	}
}

func TestPoolDelegatorSubmitTaskRejectsWithoutPublishingOrStarting(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	collector := &eventCollector{}
	runner := childRunnerFunc(func(ctx context.Context, _ ChildRunInput) ChildRunResult {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return ChildRunResult{Status: DelegationCanceled}
	})
	d, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 1}, runner, collector)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.SubmitTask(context.Background(), validParent(), tasks(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	<-started
	secondTask := DelegationTask{ID: "second", Name: "second", Instruction: "inspect"}
	second, err := d.SubmitTask(context.Background(), validParent(), secondTask)
	if err != nil {
		t.Fatal(err)
	}
	before := len(collector.snapshot())
	thirdTask := DelegationTask{ID: "rejected", Name: "rejected", Instruction: "inspect"}
	if rejected, err := d.SubmitTask(context.Background(), validParent(), thirdTask); rejected != nil || !errors.Is(err, ErrDelegationQueueFull) {
		t.Fatalf("rejected handle=%+v err=%v", rejected, err)
	}
	if got := len(collector.snapshot()); got != before {
		t.Fatalf("rejected request emitted %d events", got-before)
	}
	second.Cancel()
	first.Cancel()
	if result := awaitSubmittedResult(t, first); result.Status != DelegationCanceled {
		t.Fatalf("first result=%+v", result)
	}
	if result := awaitSubmittedResult(t, second); result.Status != DelegationCanceled {
		t.Fatalf("queued cancel result=%+v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner invoked %d times; queued canceled or rejected task started", calls.Load())
	}
	if _, ok := <-second.Results; ok {
		t.Fatal("task delivered more than one terminal result")
	}
}

func TestPoolDelegatorSubmitTaskRequiresQueuedPersistence(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	persistErr := errors.New("disk unavailable")
	reporter := delegationReporterFunc(func(_ string, event DelegationEvent) error {
		if fail.Load() && event.Status == DelegationQueued {
			return persistErr
		}
		return nil
	})
	d, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 1}, childRunnerFunc(func(context.Context, ChildRunInput) ChildRunResult {
		calls.Add(1)
		return ChildRunResult{Status: DelegationSucceeded}
	}), reporter)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if handle, err := d.SubmitTask(context.Background(), validParent(), tasks(1)[0]); handle != nil || !errors.Is(err, persistErr) {
		t.Fatalf("unpersisted handle=%+v err=%v", handle, err)
	}
	if calls.Load() != 0 || len(d.queueSlots) != 0 || len(d.queue) != 0 {
		t.Fatal("failed persistence started work or leaked queue capacity")
	}
	fail.Store(false)
	handle, err := d.SubmitTask(context.Background(), validParent(), tasks(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitSubmittedResult(t, handle); result.Status != DelegationSucceeded {
		t.Fatalf("after persistence recovery result=%+v", result)
	}
}

func TestPoolDelegatorCloseDuringQueuedPersistenceSettlesAcceptedTask(t *testing.T) {
	persisting := make(chan struct{})
	releasePersistence := make(chan struct{})
	var calls atomic.Int32
	reporter := delegationReporterFunc(func(_ string, event DelegationEvent) error {
		if event.Status == DelegationQueued {
			close(persisting)
			<-releasePersistence
		}
		return nil
	})
	d, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 1}, childRunnerFunc(func(context.Context, ChildRunInput) ChildRunResult {
		calls.Add(1)
		return ChildRunResult{Status: DelegationSucceeded}
	}), reporter)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	handles := make(chan *TaskHandle, 1)
	errs := make(chan error, 1)
	go func() {
		handle, err := d.SubmitTask(context.Background(), validParent(), tasks(1)[0])
		handles <- handle
		errs <- err
	}()
	<-persisting
	closed := make(chan struct{})
	go func() {
		d.Close()
		close(closed)
	}()
	select {
	case <-d.life.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not cancel service lifetime")
	}
	close(releasePersistence)
	if err := <-errs; err != nil {
		t.Fatalf("in-flight durable acceptance failed: %v", err)
	}
	handle := <-handles
	if result := awaitSubmittedResult(t, handle); result.Status != DelegationInterrupted {
		t.Fatalf("shutdown result=%+v", result)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("service close did not return")
	}
	if calls.Load() != 0 {
		t.Fatal("shutdown task reached the runner")
	}
	if handle, err := d.SubmitTask(context.Background(), validParent(), tasks(1)[0]); handle != nil || !errors.Is(err, ErrDelegationClosed) {
		t.Fatalf("closed service handle=%+v err=%v", handle, err)
	}
}

func TestPoolDelegatorTaskBudgetCanOnlyNarrow(t *testing.T) {
	defaults := DefaultDelegationLimits()
	larger := defaults
	larger.MaxInputBytes *= 2
	larger.MaxToolRounds *= 2
	larger.MaxDuration *= 2
	larger.MaxSummaryBytes *= 2
	larger.MaxToolOutputBytes *= 2
	gotBudgets := make(chan DelegationLimits, 2)
	d, err := NewPoolDelegator(larger, childRunnerFunc(func(_ context.Context, input ChildRunInput) ChildRunResult {
		gotBudgets <- input.Budget
		return ChildRunResult{Status: DelegationSucceeded, Summary: "long summary"}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parent := validParent()
	parent.Budget = larger
	handle, err := d.SubmitTask(context.Background(), parent, tasks(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	awaitSubmittedResult(t, handle)
	if got := <-gotBudgets; got != defaults {
		t.Fatalf("expanded budgets accepted: %+v, want %+v", got, defaults)
	}
	parent.Budget = DelegationLimits{MaxToolRounds: 2, MaxDuration: time.Second, MaxSummaryBytes: 4, MaxToolOutputBytes: 100}
	handle, err = d.SubmitTask(context.Background(), parent, tasks(1)[0])
	if err != nil {
		t.Fatal(err)
	}
	result := awaitSubmittedResult(t, handle)
	got := <-gotBudgets
	if got.MaxToolRounds != 2 || got.MaxDuration != time.Second || got.MaxSummaryBytes != 4 || got.MaxToolOutputBytes != 100 || len(result.Summary) > 4 {
		t.Fatalf("narrowed budget=%+v result=%+v", got, result)
	}
	parent.Budget.MaxInputBytes = 8
	if handle, err := d.SubmitTask(context.Background(), parent, tasks(1)[0]); handle != nil || err == nil {
		t.Fatalf("per-task input cap ignored: handle=%+v err=%v", handle, err)
	}
}

func TestNamedTaskPreboundChildRunIDIsPreservedAndRejectsDuplicate(t *testing.T) {
	entered := make(chan ChildRunInput, 1)
	release := make(chan struct{})
	runner := childRunnerFunc(func(ctx context.Context, input ChildRunInput) ChildRunResult {
		entered <- input
		select {
		case <-release:
			return ChildRunResult{Status: DelegationSucceeded}
		case <-ctx.Done():
			return ChildRunResult{Status: DelegationCanceled}
		}
	})
	pool, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 2}, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	parent := validParent()
	parent.RunID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	parent.ChildRunID = parent.RunID
	parent.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": parent.RunID, "session_id": parent.Work.SessionID, "allowed_root": parent.ProjectRoot})
	handle, err := pool.SubmitTask(context.Background(), parent, DelegationTask{ID: "first", Name: "writer", Instruction: "write"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-entered:
		if input.ChildRunID != parent.RunID {
			t.Fatalf("lease run ID changed: %s", input.ChildRunID)
		}
		var bounds struct {
			RunID string `json:"run_id"`
		}
		json.Unmarshal(input.PermissionBounds, &bounds)
		if bounds.RunID != parent.RunID {
			t.Fatalf("authority changed: %s", bounds.RunID)
		}
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	if _, err := pool.SubmitTask(context.Background(), parent, DelegationTask{ID: "duplicate", Name: "writer", Instruction: "write"}); err == nil {
		t.Fatal("duplicate active prebound ID admitted")
	}
	invalid := parent
	invalid.ChildRunID = "bad"
	if _, err := pool.SubmitTask(context.Background(), invalid, DelegationTask{ID: "invalid", Name: "writer", Instruction: "write"}); err == nil {
		t.Fatal("invalid prebound ID admitted")
	}
	close(release)
	if result := <-handle.Results; result.Status != DelegationSucceeded {
		t.Fatalf("first result=%+v", result)
	}
}

func TestTeamTurnAcceptsDistinctPreboundChildRunIDAndRejectsCollision(t *testing.T) {
	const childID = "0123456789abcdef0123456789abcdef"
	entered := make(chan ChildRunInput, 1)
	release := make(chan struct{})
	pool, err := NewPoolDelegator(DelegationLimits{Workers: 1, QueueCapacity: 2}, childRunnerFunc(func(ctx context.Context, input ChildRunInput) ChildRunResult {
		entered <- input
		select {
		case <-release:
			return ChildRunResult{Status: DelegationSucceeded}
		case <-ctx.Done():
			return ChildRunResult{Status: DelegationCanceled}
		}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	parent := validParent()
	parent.ChildRunID = childID
	parent.TeamTurn = &TeamTurnIdentity{TeamID: "team-1", MemberID: "member-1", TurnID: "turn-1", MemberName: "researcher"}
	parent.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": parent.RunID, "session_id": parent.Work.SessionID, "allowed_root": parent.ProjectRoot})
	var admittedID string
	handle, err := pool.SubmitTaskCommitted(context.Background(), parent, tasks(1)[0], func(admission TaskAdmission) error {
		admittedID = admission.ChildRunID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if admittedID != childID {
		t.Fatalf("admitted child run ID=%q, want prebound %q", admittedID, childID)
	}
	select {
	case input := <-entered:
		if input.ChildRunID != childID {
			t.Fatalf("runner child run ID=%q, want %q", input.ChildRunID, childID)
		}
		var bounds struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(input.PermissionBounds, &bounds); err != nil || bounds.RunID != childID {
			t.Fatalf("child authority run ID=%q err=%v, want %q", bounds.RunID, err, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("team child did not start")
	}

	if duplicate, err := pool.SubmitTask(context.Background(), parent, tasks(1)[0]); err == nil || duplicate != nil {
		t.Fatalf("active prebound child ID collision admitted: handle=%v err=%v", duplicate, err)
	}

	sameAsParent := parent
	sameAsParent.RunID = "fedcba9876543210fedcba9876543210"
	sameAsParent.ChildRunID = sameAsParent.RunID
	sameAsParent.PermissionBounds, _ = json.Marshal(map[string]any{"run_id": sameAsParent.RunID, "session_id": parent.Work.SessionID, "allowed_root": parent.ProjectRoot})
	if invalid, err := pool.SubmitTask(context.Background(), sameAsParent, tasks(1)[0]); err == nil || invalid != nil {
		t.Fatalf("team child reused parent run ID: handle=%v err=%v", invalid, err)
	}

	nonTeamDistinct := validParent()
	nonTeamDistinct.ChildRunID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if invalid, err := pool.SubmitTask(context.Background(), nonTeamDistinct, tasks(1)[0]); err == nil || invalid != nil {
		t.Fatalf("named task accepted a distinct prebound child ID: handle=%v err=%v", invalid, err)
	}

	close(release)
	if result := <-handle.Results; result.Status != DelegationSucceeded {
		t.Fatalf("team result=%+v", result)
	}
}
