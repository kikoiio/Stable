package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"stable/internal/llm"
)

type DelegationStatus string

const (
	DelegationQueued      DelegationStatus = "queued"
	DelegationRunning     DelegationStatus = "running"
	DelegationSucceeded   DelegationStatus = "succeeded"
	DelegationFailed      DelegationStatus = "failed"
	DelegationCanceled    DelegationStatus = "canceled"
	DelegationInterrupted DelegationStatus = "interrupted"
)

func (s DelegationStatus) IsTerminal() bool {
	switch s {
	case DelegationSucceeded, DelegationFailed, DelegationCanceled, DelegationInterrupted:
		return true
	default:
		return false
	}
}

type DelegationTask struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
}

type DelegationResult struct {
	TaskID     string           `json:"task_id"`
	ChildRunID string           `json:"child_run_id,omitempty"`
	Name       string           `json:"name"`
	Status     DelegationStatus `json:"status"`
	Summary    string           `json:"summary,omitempty"`
	Error      string           `json:"error,omitempty"`
}

type DelegationLimits struct {
	Workers            int           `json:"workers"`
	QueueCapacity      int           `json:"queue_capacity"`
	MaxInputBytes      int           `json:"max_input_bytes"`
	MaxToolRounds      int           `json:"max_tool_rounds"`
	MaxDuration        time.Duration `json:"max_duration"`
	MaxSummaryBytes    int           `json:"max_summary_bytes"`
	MaxToolOutputBytes int           `json:"max_tool_output_bytes"`
}

func DefaultDelegationLimits() DelegationLimits {
	return DelegationLimits{
		Workers: 3, QueueCapacity: 32, MaxInputBytes: 64 << 10,
		MaxToolRounds: 8, MaxDuration: 3 * time.Minute,
		MaxSummaryBytes: 8 << 10, MaxToolOutputBytes: 50_000,
	}
}

func (l DelegationLimits) normalized() DelegationLimits {
	d := DefaultDelegationLimits()
	if l.Workers > 0 {
		d.Workers = l.Workers
	}
	if l.QueueCapacity > 0 {
		d.QueueCapacity = l.QueueCapacity
	}
	if l.MaxInputBytes > 0 && l.MaxInputBytes < d.MaxInputBytes {
		d.MaxInputBytes = l.MaxInputBytes
	}
	if l.MaxToolRounds > 0 && l.MaxToolRounds < d.MaxToolRounds {
		d.MaxToolRounds = l.MaxToolRounds
	}
	if l.MaxDuration > 0 && l.MaxDuration < d.MaxDuration {
		d.MaxDuration = l.MaxDuration
	}
	if l.MaxSummaryBytes > 0 && l.MaxSummaryBytes < d.MaxSummaryBytes {
		d.MaxSummaryBytes = l.MaxSummaryBytes
	}
	if l.MaxToolOutputBytes > 0 && l.MaxToolOutputBytes < d.MaxToolOutputBytes {
		d.MaxToolOutputBytes = l.MaxToolOutputBytes
	}
	return d
}

// narrowed keeps pool capacity unchanged and permits only smaller per-task budgets.
func (l DelegationLimits) narrowed(request DelegationLimits) DelegationLimits {
	r := request.normalized()
	l.MaxInputBytes = min(l.MaxInputBytes, r.MaxInputBytes)
	l.MaxToolRounds = min(l.MaxToolRounds, r.MaxToolRounds)
	l.MaxDuration = min(l.MaxDuration, r.MaxDuration)
	l.MaxSummaryBytes = min(l.MaxSummaryBytes, r.MaxSummaryBytes)
	l.MaxToolOutputBytes = min(l.MaxToolOutputBytes, r.MaxToolOutputBytes)
	return l
}

type ParentRun struct {
	TeamTurn         *TeamTurnIdentity
	TeamLifecycle    *TeamRunLifecycle
	RoleInstruction  string
	ToolCallID       string
	RunID            string
	Deadline         time.Time
	Budget           DelegationLimits
	Work             WorkRef
	ProjectRoot      string
	PermissionBounds json.RawMessage
	Provider         llm.Provider
	ProviderName     string
	Model            string
	ToolSchemas      []llm.ToolSchema
	ExecutorFactory  ExecutorFactory
}

type Delegator interface {
	RunBatch(context.Context, ParentRun, []DelegationTask) ([]DelegationResult, error)
	RunTask(context.Context, ParentRun, DelegationTask) (DelegationResult, error)
}

type TaskHandle struct {
	BatchID, TaskID string
	Results         <-chan DelegationResult
	Cancel          context.CancelFunc
}

type TaskSubmitter interface {
	SubmitTask(context.Context, ParentRun, DelegationTask) (*TaskHandle, error)
}

// TaskAdmission identifies a tentative reservation, not an accepted task.
// The service must not announce success until SubmitTaskCommitted returns.
type TaskAdmission struct {
	BatchID, TaskID string
	ChildRunID      string
}

// TeamRunLifecycle writes durable child-run attribution before a provider can
// execute and records the child outcome before the accepted team turn closes.
// It is present only on service-created team turns; model input cannot set it.
type TeamRunLifecycle struct {
	Started  func(ChildRunInput) error
	Finished func(ChildRunInput, ChildRunResult) error
}

type CommittedTaskSubmitter interface {
	SubmitTaskCommitted(context.Context, ParentRun, DelegationTask, func(TaskAdmission) error) (*TaskHandle, error)
	CapacityChanged() <-chan struct{}
}

var (
	ErrDelegationQueueFull = errors.New("delegation queue is full")
	ErrDelegationClosed    = errors.New("delegation service is shutting down")
)

type ChildRunInput struct {
	TeamTurn         *TeamTurnIdentity
	RoleInstruction  string
	ParentRunID      string
	BatchID          string
	ChildRunID       string
	Work             WorkRef
	Task             DelegationTask
	ProjectRoot      string
	PermissionBounds json.RawMessage
	Provider         llm.Provider
	ProviderName     string
	Model            string
	Budget           DelegationLimits
	ToolSchemas      []llm.ToolSchema
	ExecutorFactory  ExecutorFactory
	Progress         DelegationProgress
}

type ChildRunResult struct {
	Status  DelegationStatus
	Summary string
	Error   string
}

type ChildRunner interface {
	Run(context.Context, ChildRunInput) ChildRunResult
}

type delegationWork struct {
	parent        ParentRun
	parentContext context.Context
	cancel        context.CancelFunc
	batchID       string
	task          DelegationTask
	childRunID    string
	result        chan DelegationResult
}

// PoolDelegator is a service-scoped bounded FIFO scheduler. Its queue is
// shared by all parent session runs using this instance.
type PoolDelegator struct {
	limits          DelegationLimits
	runner          ChildRunner
	reporter        ProgressReporter
	queue           chan delegationWork
	queueSlots      chan struct{}
	capacityChanged chan struct{}
	newID           func() (string, error)
	life            context.Context
	cancel          context.CancelFunc
	submitMu        sync.Mutex
	closed          bool
	closeOnce       sync.Once
}

func NewPoolDelegator(limits DelegationLimits, runner ChildRunner, reporter ProgressReporter) (*PoolDelegator, error) {
	if runner == nil {
		return nil, errors.New("delegation child runner is required")
	}
	limits = limits.normalized()
	if limits.Workers < 1 || limits.QueueCapacity < 1 {
		return nil, errors.New("delegation worker and queue limits must be positive")
	}
	life, cancel := context.WithCancel(context.Background())
	d := &PoolDelegator{
		limits: limits, runner: runner, reporter: reporter,
		queue:           make(chan delegationWork, limits.QueueCapacity),
		queueSlots:      make(chan struct{}, limits.QueueCapacity),
		capacityChanged: make(chan struct{}, 1),
		newID:           randomDelegationID, life: life, cancel: cancel,
	}
	for i := 0; i < limits.Workers; i++ {
		go d.worker()
	}
	return d, nil
}

func randomDelegationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (d *PoolDelegator) SetIDGeneratorForTest(fn func() (string, error)) {
	if fn != nil {
		d.newID = fn
	}
}

func validateDelegationBatch(parent ParentRun, tasks []DelegationTask, maxBytes int) error {
	if err := validateDelegationParent(parent, false); err != nil {
		return err
	}
	return validateDelegationTasks(tasks, maxBytes)
}

func validateDelegationParent(parent ParentRun, allowGoal bool) error {
	if parent.TeamTurn != nil && parent.TeamTurn.Validate() != nil {
		return errors.New("delegation team turn identity is invalid")
	}
	if parent.RunID == "" || parent.Work.SessionID == "" {
		return errors.New("delegation requires a session work run")
	}
	switch parent.Work.Kind {
	case WorkSession:
	case WorkGoal:
		if !allowGoal {
			return errors.New("delegation requires a session work run")
		}
		if strings.TrimSpace(parent.Work.GoalID) == "" {
			return errors.New("goal delegation requires a goal ID")
		}
	default:
		return fmt.Errorf("delegation work kind %q is not supported", parent.Work.Kind)
	}
	if parent.Provider == nil || parent.Model == "" || parent.ProjectRoot == "" {
		return errors.New("delegation parent is missing provider, model, or project root")
	}
	return nil
}

func validateDelegationTasks(tasks []DelegationTask, maxBytes int) error {
	if len(tasks) == 0 {
		return errors.New("delegation requires at least one task")
	}
	seen := make(map[string]struct{}, len(tasks))
	for i, task := range tasks {
		if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Name) == "" || strings.TrimSpace(task.Instruction) == "" {
			return fmt.Errorf("task %d requires id, name, and instruction", i+1)
		}
		if _, ok := seen[task.ID]; ok {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = struct{}{}
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		return fmt.Errorf("encode delegation tasks: %w", err)
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("delegation input exceeds %d bytes", maxBytes)
	}
	return nil
}

func (d *PoolDelegator) RunBatch(ctx context.Context, parent ParentRun, tasks []DelegationTask) ([]DelegationResult, error) {
	if d == nil {
		return nil, errors.New("delegator is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDelegationBatch(parent, tasks, d.limits.narrowed(parent.Budget).MaxInputBytes); err != nil {
		return nil, err
	}
	return d.runTasks(ctx, parent, tasks)
}

// RunTask submits one child work item to the same bounded FIFO pool as
// RunBatch. Hook agents use this path for Session and Goal parent runs; the
// public batch path remains Session-only.
func (d *PoolDelegator) RunTask(ctx context.Context, parent ParentRun, task DelegationTask) (DelegationResult, error) {
	handle, err := d.SubmitTask(ctx, parent, task)
	if err != nil {
		status := DelegationFailed
		switch {
		case errors.Is(err, ErrDelegationClosed):
			status = DelegationInterrupted
		case errors.Is(err, ErrDelegationQueueFull):
		default:
			return DelegationResult{}, err
		}
		batchID, idErr := d.newID()
		if idErr != nil {
			return DelegationResult{}, fmt.Errorf("create delegation batch id: %w", idErr)
		}
		if publishErr := d.publish(parent.RunID, parent.Work.SessionID, DelegationEvent{BatchID: batchID, TaskID: task.ID, TaskName: task.Name, Status: DelegationQueued}); publishErr != nil {
			return DelegationResult{}, fmt.Errorf("publish queued event for %q: %w", task.ID, publishErr)
		}
		result := DelegationResult{TaskID: task.ID, Name: task.Name, Status: status, Error: err.Error()}
		_ = d.publish(parent.RunID, parent.Work.SessionID, DelegationEvent{BatchID: batchID, TaskID: task.ID, TaskName: task.Name, Status: status, Error: result.Error})
		return result, nil
	}
	defer handle.Cancel()
	return <-handle.Results, nil
}

// SubmitTask accepts one item without waiting for a worker or queue capacity.
// A reservation keeps other producers from taking its slot while queued is
// persisted. No runner can observe the work before persistence succeeds.
func (d *PoolDelegator) SubmitTask(ctx context.Context, parent ParentRun, task DelegationTask) (*TaskHandle, error) {
	return d.SubmitTaskCommitted(ctx, parent, task, nil)
}

// SubmitTaskCommitted reserves a real queue slot before committing service
// facts, then persists queued and exposes the work to a worker. The callback
// runs synchronously under the submission lock and must not call this pool.
// Callback, cancellation or queued-persistence failure releases the tentative
// slot without starting a provider; caller owns durable abort/terminal facts.
func (d *PoolDelegator) SubmitTaskCommitted(ctx context.Context, parent ParentRun, task DelegationTask, commit func(TaskAdmission) error) (*TaskHandle, error) {
	if d == nil {
		return nil, errors.New("delegator is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent.TeamTurn = cloneTeamTurnIdentity(parent.TeamTurn)
	if err := validateDelegationParent(parent, true); err != nil {
		return nil, err
	}
	if err := validateDelegationTasks([]DelegationTask{task}, d.limits.narrowed(parent.Budget).MaxInputBytes); err != nil {
		return nil, err
	}
	if d.life.Err() != nil {
		return nil, ErrDelegationClosed
	}
	select {
	case d.queueSlots <- struct{}{}:
	default:
		return nil, ErrDelegationQueueFull
	}
	accepted := false
	defer func() {
		if !accepted {
			d.releaseQueueSlot()
		}
	}()
	d.submitMu.Lock()
	defer d.submitMu.Unlock()
	if d.closed || d.life.Err() != nil {
		return nil, ErrDelegationClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batchID, err := d.newID()
	if err != nil {
		return nil, fmt.Errorf("create delegation batch id: %w", err)
	}
	childRunID := ""
	if parent.TeamTurn != nil {
		childRunID, err = d.newID()
		if err != nil {
			return nil, fmt.Errorf("create child run ID: %w", err)
		}
	}
	if commit != nil {
		if err := commit(TaskAdmission{BatchID: batchID, TaskID: task.ID, ChildRunID: childRunID}); err != nil {
			return nil, fmt.Errorf("commit delegation admission: %w", err)
		}
		if d.life.Err() != nil {
			return nil, ErrDelegationClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if err := d.publish(parent.RunID, parent.Work.SessionID, DelegationEvent{BatchID: batchID, TaskID: task.ID, TaskName: task.Name, Status: DelegationQueued}); err != nil {
		return nil, fmt.Errorf("publish queued event for %q: %w", task.ID, err)
	}
	workCtx, cancel := context.WithCancel(ctx)
	results := make(chan DelegationResult, 1)
	d.queue <- delegationWork{parent: parent, parentContext: workCtx, cancel: cancel, batchID: batchID, task: task, childRunID: childRunID, result: results}
	accepted = true
	return &TaskHandle{BatchID: batchID, TaskID: task.ID, Results: results, Cancel: cancel}, nil
}

// CapacityChanged is one coalescing signal for the service's single team
// scheduler. It reports released reservations/dequeued work and shutdown;
// callers still use nonblocking admission and keep their own fair ready set.
// It is deliberately not a per-member subscription or an unbounded queue.
func (d *PoolDelegator) CapacityChanged() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.capacityChanged
}

func (d *PoolDelegator) notifyCapacity() {
	select {
	case d.capacityChanged <- struct{}{}:
	default:
	}
}

func (d *PoolDelegator) releaseQueueSlot() {
	<-d.queueSlots
	d.notifyCapacity()
}

func (d *PoolDelegator) runTasks(ctx context.Context, parent ParentRun, tasks []DelegationTask) ([]DelegationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batchID, err := d.newID()
	if err != nil {
		return nil, fmt.Errorf("create delegation batch id: %w", err)
	}
	results := make([]DelegationResult, len(tasks))
	resultChans := make([]chan DelegationResult, len(tasks))
	queued := make([]bool, len(tasks))
	fillTerminal := func(start int, status DelegationStatus, reason string, batchID string) {
		for j := start; j < len(tasks); j++ {
			results[j] = DelegationResult{TaskID: tasks[j].ID, Name: tasks[j].Name, Status: status, Error: reason}
			_ = d.publish(parent.RunID, parent.Work.SessionID, DelegationEvent{BatchID: batchID, TaskID: tasks[j].ID, TaskName: tasks[j].Name, Status: status, Error: reason})
		}
	}
enqueueLoop:
	for i, task := range tasks {
		if err = d.publish(parent.RunID, parent.Work.SessionID, DelegationEvent{BatchID: batchID, TaskID: task.ID, TaskName: task.Name, Status: DelegationQueued}); err != nil {
			return nil, fmt.Errorf("publish queued event for %q: %w", task.ID, err)
		}
		ch := make(chan DelegationResult, 1)
		resultChans[i] = ch
		work := delegationWork{parent: parent, parentContext: ctx, batchID: batchID, task: task, result: ch}
		// Waiting for queue space happens outside submitMu so shutdown and
		// nonblocking single-task submissions remain independent of batch pressure.
		select {
		case d.queueSlots <- struct{}{}:
		case <-ctx.Done():
			fillTerminal(i, DelegationCanceled, ctx.Err().Error(), batchID)
			break enqueueLoop
		case <-d.life.Done():
			fillTerminal(i, DelegationInterrupted, ErrDelegationClosed.Error(), batchID)
			break enqueueLoop
		}
		d.submitMu.Lock()
		if d.closed || d.life.Err() != nil {
			d.submitMu.Unlock()
			d.releaseQueueSlot()
			fillTerminal(i, DelegationInterrupted, ErrDelegationClosed.Error(), batchID)
			break
		}
		if err := ctx.Err(); err != nil {
			d.submitMu.Unlock()
			d.releaseQueueSlot()
			fillTerminal(i, DelegationCanceled, err.Error(), batchID)
			break
		}
		d.queue <- work
		queued[i] = true
		d.submitMu.Unlock()
		if ctx.Err() != nil {
			fillTerminal(i+1, DelegationCanceled, ctx.Err().Error(), batchID)
			break
		}
	}
	for i := range tasks {
		if queued[i] {
			results[i] = <-resultChans[i]
		}
	}
	return results, nil
}

func (d *PoolDelegator) publish(parentRunID, sessionID string, event DelegationEvent) error {
	if d.reporter == nil {
		return nil
	}
	event.SessionID = sessionID
	event.UpdatedAt = time.Now().UTC()
	return d.reporter.Publish(parentRunID, event)
}

func (d *PoolDelegator) finishWork(work delegationWork, result DelegationResult) {
	if work.cancel != nil {
		work.cancel()
	}
	work.result <- result
	close(work.result)
}

func (d *PoolDelegator) worker() {
	for {
		select {
		case work := <-d.queue:
			d.releaseQueueSlot()
			d.finishWork(work, d.runWork(work))
		case <-d.life.Done():
			// Producers may be persisting a queued event when life is canceled.
			// Wait for their enqueue before draining, so every accepted handle ends.
			d.submitMu.Lock()
			var pending []delegationWork
			for {
				select {
				case work := <-d.queue:
					d.releaseQueueSlot()
					pending = append(pending, work)
				default:
					d.submitMu.Unlock()
					for _, work := range pending {
						d.finishWork(work, d.terminal(work, DelegationInterrupted, "", "delegation service interrupted"))
					}
					return
				}
			}
		}
	}
}

func (d *PoolDelegator) runWork(work delegationWork) DelegationResult {
	ctx := work.parentContext
	budget := d.limits.narrowed(work.parent.Budget)
	// Record and pass the child budget already narrowed to the caller's
	// remaining deadline, so the runner cannot observe a larger allowance.
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	if !work.parent.Deadline.IsZero() {
		if remaining := time.Until(work.parent.Deadline); remaining > 0 && remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	if work.childRunID == "" {
		var err error
		work.childRunID, err = d.newID()
		if err != nil {
			return d.terminal(work, DelegationFailed, "", "could not create child run ID")
		}
	}
	input, inputErr := d.childInput(work, budget)
	if inputErr != nil {
		return d.terminal(work, DelegationFailed, "", "child run authority is invalid")
	}
	if work.parent.TeamLifecycle != nil && work.parent.TeamLifecycle.Started != nil {
		if err := work.parent.TeamLifecycle.Started(input); err != nil {
			return d.terminal(work, DelegationFailed, "", "could not persist team child run start")
		}
	}
	finishTeamRun := func(result ChildRunResult) DelegationResult {
		if !result.Status.IsTerminal() {
			if result.Error != "" {
				result.Status = DelegationFailed
			} else {
				result.Status = DelegationSucceeded
			}
		}
		if work.parent.TeamLifecycle != nil && work.parent.TeamLifecycle.Finished != nil {
			if err := work.parent.TeamLifecycle.Finished(input, result); err != nil {
				return d.terminal(work, DelegationFailed, result.Summary, "could not persist team child run outcome")
			}
		}
		return d.terminal(work, result.Status, result.Summary, result.Error)
	}
	if d.life.Err() != nil {
		return finishTeamRun(ChildRunResult{Status: DelegationInterrupted, Error: "delegation service interrupted"})
	}
	if err := ctx.Err(); err != nil {
		return finishTeamRun(ChildRunResult{Status: DelegationCanceled, Error: err.Error()})
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return finishTeamRun(ChildRunResult{Status: DelegationCanceled, Error: context.DeadlineExceeded.Error()})
		}
		if remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	if !work.parent.Deadline.IsZero() {
		remaining := time.Until(work.parent.Deadline)
		if remaining <= 0 {
			return finishTeamRun(ChildRunResult{Status: DelegationFailed, Error: "parent run budget exhausted"})
		}
		if remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	childCtx, cancel := context.WithTimeout(ctx, budget.MaxDuration)
	stopLife := context.AfterFunc(d.life, cancel)
	defer cancel()
	defer stopLife()
	if err := d.publish(work.parent.RunID, work.parent.Work.SessionID, DelegationEvent{BatchID: work.batchID, TaskID: work.task.ID, TaskName: work.task.Name, Status: DelegationRunning}); err != nil {
		return finishTeamRun(ChildRunResult{Status: DelegationFailed, Error: "could not record child start"})
	}
	var progressMu sync.Mutex
	var progressErr error
	input.Progress = func(stage, summary string) {
		summary = SanitizeRoleOutput(summary, work.parent.RoleInstruction)
		if len(summary) > budget.MaxSummaryBytes {
			summary = truncateUTF8(summary, budget.MaxSummaryBytes)
		}
		if err := d.publish(work.parent.RunID, work.parent.Work.SessionID, DelegationEvent{
			BatchID: work.batchID, TaskID: work.task.ID, TaskName: work.task.Name,
			Status: DelegationRunning, Stage: stage, Summary: summary,
		}); err != nil {
			progressMu.Lock()
			if progressErr == nil {
				progressErr = err
			}
			progressMu.Unlock()
		}
	}
	child := d.runner.Run(childCtx, input)
	progressMu.Lock()
	progressFailed := progressErr != nil
	progressMu.Unlock()
	if progressFailed {
		child.Status, child.Error = DelegationFailed, "could not record child progress"
	}
	if d.life.Err() != nil && !child.Status.IsTerminal() {
		child.Status, child.Error = DelegationInterrupted, "delegation service interrupted"
	}
	if childCtx.Err() != nil && (child.Status == "" || child.Status == DelegationRunning) {
		status := DelegationCanceled
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			status = DelegationFailed
		}
		child.Status, child.Error = status, childCtx.Err().Error()
	}
	child.Summary = truncateUTF8(child.Summary, budget.MaxSummaryBytes)
	return finishTeamRun(child)
}

func (d *PoolDelegator) childInput(work delegationWork, budget DelegationLimits) (ChildRunInput, error) {
	childID := work.childRunID
	var err error
	var authority map[string]json.RawMessage
	if json.Unmarshal(work.parent.PermissionBounds, &authority) != nil {
		return ChildRunInput{}, errors.New("parent permission bounds are invalid")
	}
	var parentAuthorityRunID string
	if json.Unmarshal(authority["run_id"], &parentAuthorityRunID) != nil || parentAuthorityRunID != work.parent.RunID {
		return ChildRunInput{}, errors.New("parent permission bounds are invalid")
	}
	authority["run_id"], _ = json.Marshal(childID)
	childBounds, err := json.Marshal(authority)
	if err != nil {
		return ChildRunInput{}, err
	}
	return ChildRunInput{
		TeamTurn: cloneTeamTurnIdentity(work.parent.TeamTurn), ParentRunID: work.parent.RunID,
		BatchID: work.batchID, ChildRunID: childID, Task: work.task, RoleInstruction: work.parent.RoleInstruction,
		Work: work.parent.Work, ProjectRoot: work.parent.ProjectRoot, Provider: work.parent.Provider,
		PermissionBounds: childBounds, ProviderName: work.parent.ProviderName, Model: work.parent.Model,
		Budget: budget, ToolSchemas: append([]llm.ToolSchema(nil), work.parent.ToolSchemas...),
		ExecutorFactory: work.parent.ExecutorFactory,
	}, nil
}

func (d *PoolDelegator) terminal(work delegationWork, status DelegationStatus, summary, errText string) DelegationResult {
	summary = truncateUTF8(SanitizeRoleOutput(summary, work.parent.RoleInstruction), d.limits.narrowed(work.parent.Budget).MaxSummaryBytes)
	errText = SanitizeRoleOutput(errText, work.parent.RoleInstruction)
	if !status.IsTerminal() {
		status = DelegationFailed
		if errText == "" {
			errText = "child returned a non-terminal status"
		}
	}
	result := DelegationResult{TaskID: work.task.ID, ChildRunID: work.childRunID, Name: work.task.Name, Status: status, Summary: summary, Error: errText}
	if err := d.publish(work.parent.RunID, work.parent.Work.SessionID, DelegationEvent{BatchID: work.batchID, TaskID: work.task.ID, TaskName: work.task.Name, Status: status, Summary: summary, Error: errText}); err != nil && result.Error == "" {
		result.Status = DelegationFailed
		result.Error = "could not record child result"
	}
	return result
}

func truncateUTF8(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	const ellipsis = "…"
	if maxBytes < len(ellipsis) {
		return strings.Repeat(".", maxBytes)
	}
	cut := maxBytes - len(ellipsis)
	text = text[:cut]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return text + ellipsis
}

var _ Delegator = (*PoolDelegator)(nil)
var _ TaskSubmitter = (*PoolDelegator)(nil)
var _ CommittedTaskSubmitter = (*PoolDelegator)(nil)

func (d *PoolDelegator) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		d.cancel()
		d.notifyCapacity()
		d.submitMu.Lock()
		d.closed = true
		d.submitMu.Unlock()
	})
}
