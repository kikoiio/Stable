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
	TaskID  string           `json:"task_id"`
	Name    string           `json:"name"`
	Status  DelegationStatus `json:"status"`
	Summary string           `json:"summary,omitempty"`
	Error   string           `json:"error,omitempty"`
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
	if l.MaxInputBytes > 0 {
		d.MaxInputBytes = l.MaxInputBytes
	}
	if l.MaxToolRounds > 0 {
		d.MaxToolRounds = l.MaxToolRounds
	}
	if l.MaxDuration > 0 {
		d.MaxDuration = l.MaxDuration
	}
	if l.MaxSummaryBytes > 0 {
		d.MaxSummaryBytes = l.MaxSummaryBytes
	}
	if l.MaxToolOutputBytes > 0 {
		d.MaxToolOutputBytes = l.MaxToolOutputBytes
	}
	return d
}

type ParentRun struct {
	RunID            string
	Deadline         time.Time
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
}

type ChildRunInput struct {
	ParentRunID      string
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
	batchID       string
	task          DelegationTask
	result        chan DelegationResult
}

// PoolDelegator is a service-scoped bounded FIFO scheduler. Its queue is
// shared by all parent session runs using this instance.
type PoolDelegator struct {
	limits    DelegationLimits
	runner    ChildRunner
	reporter  ProgressReporter
	queue     chan delegationWork
	newID     func() (string, error)
	life      context.Context
	cancel    context.CancelFunc
	submitMu  sync.RWMutex
	closed    bool
	closeOnce sync.Once
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
		queue: make(chan delegationWork, limits.QueueCapacity),
		newID: randomDelegationID, life: life, cancel: cancel,
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
	if parent.RunID == "" || parent.Work.SessionID == "" || parent.Work.Kind != WorkSession {
		return errors.New("delegation requires a session work run")
	}
	if parent.Provider == nil || parent.Model == "" || parent.ProjectRoot == "" {
		return errors.New("delegation parent is missing provider, model, or project root")
	}
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
	if err := validateDelegationBatch(parent, tasks, d.limits.MaxInputBytes); err != nil {
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
			_ = d.publish(parent.RunID, DelegationEvent{BatchID: batchID, TaskID: tasks[j].ID, TaskName: tasks[j].Name, Status: status, Error: reason})
		}
	}
enqueueLoop:
	for i, task := range tasks {
		if err = d.publish(parent.RunID, DelegationEvent{BatchID: batchID, TaskID: task.ID, TaskName: task.Name, Status: DelegationQueued}); err != nil {
			return nil, fmt.Errorf("publish queued event for %q: %w", task.ID, err)
		}
		ch := make(chan DelegationResult, 1)
		resultChans[i] = ch
		work := delegationWork{parent: parent, parentContext: ctx, batchID: batchID, task: task, result: ch}
		d.submitMu.RLock()
		if d.closed {
			d.submitMu.RUnlock()
			fillTerminal(i, DelegationInterrupted, "delegation service is shutting down", batchID)
			break
		}
		select {
		case d.queue <- work:
			queued[i] = true
			d.submitMu.RUnlock()
		case <-ctx.Done():
			d.submitMu.RUnlock()
			fillTerminal(i, DelegationCanceled, ctx.Err().Error(), batchID)
			break enqueueLoop
		case <-d.life.Done():
			d.submitMu.RUnlock()
			fillTerminal(i, DelegationInterrupted, "delegation service is shutting down", batchID)
			break enqueueLoop
		}
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

func (d *PoolDelegator) publish(parentRunID string, event DelegationEvent) error {
	if d.reporter == nil {
		return nil
	}
	event.UpdatedAt = time.Now().UTC()
	return d.reporter.Publish(parentRunID, event)
}

func (d *PoolDelegator) worker() {
	for {
		select {
		case work := <-d.queue:
			work.result <- d.runWork(work)
		case <-d.life.Done():
			for {
				select {
				case work := <-d.queue:
					work.result <- d.terminal(work, DelegationInterrupted, "", "delegation service interrupted")
				default:
					return
				}
			}
		}
	}
}

func (d *PoolDelegator) runWork(work delegationWork) DelegationResult {
	ctx := work.parentContext
	if err := ctx.Err(); err != nil {
		return d.terminal(work, DelegationCanceled, "", err.Error())
	}
	budget := d.limits
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return d.terminal(work, DelegationCanceled, "", context.DeadlineExceeded.Error())
		}
		if remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	if !work.parent.Deadline.IsZero() {
		remaining := time.Until(work.parent.Deadline)
		if remaining <= 0 {
			return d.terminal(work, DelegationFailed, "", "parent run budget exhausted")
		}
		if remaining < budget.MaxDuration {
			budget.MaxDuration = remaining
		}
	}
	childCtx, cancel := context.WithTimeout(ctx, budget.MaxDuration)
	stopLife := context.AfterFunc(d.life, cancel)
	defer cancel()
	defer stopLife()
	if err := d.publish(work.parent.RunID, DelegationEvent{BatchID: work.batchID, TaskID: work.task.ID, TaskName: work.task.Name, Status: DelegationRunning}); err != nil {
		return d.terminal(work, DelegationFailed, "", "could not record child start")
	}
	childID, err := d.newID()
	if err != nil {
		return d.terminal(work, DelegationFailed, "", "could not create child run ID")
	}
	var authority map[string]json.RawMessage
	if json.Unmarshal(work.parent.PermissionBounds, &authority) != nil {
		return d.terminal(work, DelegationFailed, "", "parent permission bounds are invalid")
	}
	var parentAuthorityRunID string
	if json.Unmarshal(authority["run_id"], &parentAuthorityRunID) != nil || parentAuthorityRunID != work.parent.RunID {
		return d.terminal(work, DelegationFailed, "", "parent permission bounds are invalid")
	}
	authority["run_id"], _ = json.Marshal(childID)
	childBounds, err := json.Marshal(authority)
	if err != nil {
		return d.terminal(work, DelegationFailed, "", "could not derive child permission bounds")
	}
	input := ChildRunInput{
		ParentRunID: work.parent.RunID, ChildRunID: childID, Task: work.task,
		Work:        work.parent.Work,
		ProjectRoot: work.parent.ProjectRoot, Provider: work.parent.Provider,
		PermissionBounds: childBounds,
		ProviderName:     work.parent.ProviderName, Model: work.parent.Model, Budget: budget,
		ToolSchemas:     append([]llm.ToolSchema(nil), work.parent.ToolSchemas...),
		ExecutorFactory: work.parent.ExecutorFactory,
	}
	var progressMu sync.Mutex
	var progressErr error
	input.Progress = func(stage, summary string) {
		if len(summary) > budget.MaxSummaryBytes {
			summary = truncateUTF8(summary, budget.MaxSummaryBytes)
		}
		if err := d.publish(work.parent.RunID, DelegationEvent{
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
	err = progressErr
	progressMu.Unlock()
	if err != nil {
		return d.terminal(work, DelegationFailed, "", "could not record child progress")
	}
	if d.life.Err() != nil {
		return d.terminal(work, DelegationInterrupted, child.Summary, "delegation service interrupted")
	}
	if childCtx.Err() != nil && (child.Status == "" || child.Status == DelegationRunning) {
		status := DelegationCanceled
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			status = DelegationFailed
		}
		return d.terminal(work, status, child.Summary, childCtx.Err().Error())
	}
	status := child.Status
	if !status.IsTerminal() {
		if child.Error != "" {
			status = DelegationFailed
		} else {
			status = DelegationSucceeded
		}
	}
	return d.terminal(work, status, truncateUTF8(child.Summary, budget.MaxSummaryBytes), child.Error)
}

func (d *PoolDelegator) terminal(work delegationWork, status DelegationStatus, summary, errText string) DelegationResult {
	if !status.IsTerminal() {
		status = DelegationFailed
		if errText == "" {
			errText = "child returned a non-terminal status"
		}
	}
	result := DelegationResult{TaskID: work.task.ID, Name: work.task.Name, Status: status, Summary: summary, Error: errText}
	if err := d.publish(work.parent.RunID, DelegationEvent{BatchID: work.batchID, TaskID: work.task.ID, TaskName: work.task.Name, Status: status, Summary: summary, Error: errText}); err != nil && result.Error == "" {
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

func (d *PoolDelegator) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		d.cancel()
		d.submitMu.Lock()
		d.closed = true
		d.submitMu.Unlock()
	})
}
