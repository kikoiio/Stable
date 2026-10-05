// Package todo manages per-session task lists under .stable/tasks.
// A task list belongs to exactly one session and is persisted as private
// JSON that is redacted before every write. TaskList serializes every
// read-modify-write cycle behind a mutex and reports each change through an
// onChange callback so callers can journal full snapshots.
package todo

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Status is the lifecycle state of one task. StatusDeleted never persists on
// a stored task: it only appears in UpdatePatch to remove a task.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	// StatusDeleted asks Update to remove the task instead of mutating it.
	StatusDeleted Status = "deleted"
)

// MaxTasks is the maximum number of tasks one session's list may hold.
const MaxTasks = 100

// Task is one entry of a session task list.
type Task struct {
	ID          string            `json:"id"`
	Subject     string            `json:"subject"`
	Description string            `json:"description"`
	ActiveForm  string            `json:"active_form,omitempty"`
	Status      Status            `json:"status"`
	Owner       string            `json:"owner,omitempty"`
	Blocks      []string          `json:"blocks,omitempty"`
	BlockedBy   []string          `json:"blocked_by,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// UpdatePatch carries the optional modifications of one task. Nil pointers
// and nil slices leave the corresponding field untouched; AddBlocks and
// AddBlockedBy append to the existing dependencies with de-duplication,
// matching the task_update tool arguments. A patch whose Status is
// StatusDeleted removes the task.
type UpdatePatch struct {
	Subject      *string
	Description  *string
	ActiveForm   *string
	Status       *Status
	Owner        *string
	AddBlocks    []string
	AddBlockedBy []string
}

// ErrNotFound reports that no task with the requested id exists.
var ErrNotFound = errors.New("task not found")

// TaskList is the per-session task list. Every method runs load, modify,
// save and the onChange callback under one mutex, so concurrent callers
// never lose updates.
type TaskList struct {
	mu       sync.Mutex
	store    *Store
	initErr  error
	onChange func(tasks []Task) error
}

// NewTaskList returns the task list of one session. The onChange callback,
// when non-nil, receives a full snapshot of the redacted tasks after every
// successful change; an error it returns fails the mutating call. A session
// id that would escape the tasks directory is rejected on first use, so all
// methods fail consistently without touching the filesystem.
func NewTaskList(projectRoot, sessionID string, onChange func(tasks []Task) error) *TaskList {
	tl := &TaskList{onChange: onChange}
	store, err := NewStore(projectRoot, sessionID)
	if err != nil {
		tl.initErr = err
		return tl
	}
	tl.store = store
	return tl
}

// SetCredentials configures the secrets stripped from every persisted text
// field. It must be called before the first mutating use, concurrent with
// mutations it is serialized by the task list mutex.
func (tl *TaskList) SetCredentials(credentials []string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.store != nil {
		tl.store.setCredentials(credentials)
	}
}

// Create appends a new pending task and returns it. The subject is required
// and the list may hold at most MaxTasks tasks.
func (tl *TaskList) Create(subject, description, activeForm string, metadata map[string]string) (Task, error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if err := tl.ready(); err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(subject) == "" {
		return Task{}, errors.New("task subject is required")
	}
	tasks, err := tl.store.Load()
	if err != nil {
		return Task{}, err
	}
	if len(tasks) >= MaxTasks {
		return Task{}, fmt.Errorf("task list is full: at most %d tasks are allowed", MaxTasks)
	}
	task := Task{
		ID:          randomID(),
		Subject:     subject,
		Description: description,
		ActiveForm:  activeForm,
		Status:      StatusPending,
		Metadata:    copyMetadata(metadata),
	}
	tasks = append(tasks, task)
	if err := tl.commit(tasks); err != nil {
		return Task{}, err
	}
	return task, nil
}

// Get returns one task of the list or ErrNotFound.
func (tl *TaskList) Get(id string) (Task, error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tasks, err := tl.snapshot()
	if err != nil {
		return Task{}, err
	}
	for _, task := range tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// List returns every task of the session.
func (tl *TaskList) List() ([]Task, error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.snapshot()
}

// Update applies the patch to one task and returns it. Setting the status to
// StatusDeleted removes the task; deleting a task, or changing its
// dependencies, also drops every Blocks/BlockedBy entry across the list that
// no longer points at an existing task.
func (tl *TaskList) Update(id string, patch UpdatePatch) (Task, error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tasks, err := tl.snapshot()
	if err != nil {
		return Task{}, err
	}
	index := -1
	for i := range tasks {
		if tasks[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	if patch.Status != nil && *patch.Status == StatusDeleted {
		removed := tasks[index]
		remaining := append(tasks[:index:index], tasks[index+1:]...)
		remaining = dropDanglingRefs(remaining)
		if err := tl.commit(remaining); err != nil {
			return Task{}, err
		}
		return removed, nil
	}

	task := &tasks[index]
	if patch.Subject != nil {
		if strings.TrimSpace(*patch.Subject) == "" {
			return Task{}, errors.New("task subject is required")
		}
		task.Subject = *patch.Subject
	}
	if patch.Description != nil {
		task.Description = *patch.Description
	}
	if patch.ActiveForm != nil {
		task.ActiveForm = *patch.ActiveForm
	}
	if patch.Owner != nil {
		task.Owner = *patch.Owner
	}
	if patch.Status != nil {
		switch *patch.Status {
		case StatusPending, StatusInProgress, StatusCompleted:
			task.Status = *patch.Status
		default:
			return Task{}, fmt.Errorf("invalid task status %q", string(*patch.Status))
		}
	}
	dependencyChanged := len(patch.AddBlocks) > 0 || len(patch.AddBlockedBy) > 0
	if dependencyChanged {
		task.Blocks = appendUnique(task.Blocks, patch.AddBlocks)
		task.BlockedBy = appendUnique(task.BlockedBy, patch.AddBlockedBy)
		tasks = dropDanglingRefs(tasks)
	}
	if err := tl.commit(tasks); err != nil {
		return Task{}, err
	}
	return *task, nil
}

// snapshot loads the persisted tasks; the caller must hold tl.mu.
func (tl *TaskList) snapshot() ([]Task, error) {
	if err := tl.ready(); err != nil {
		return nil, err
	}
	return tl.store.Load()
}

// ready reports why the task list cannot be used; the caller must hold tl.mu.
func (tl *TaskList) ready() error {
	if tl.initErr != nil {
		return tl.initErr
	}
	if tl.store == nil {
		return errors.New("task list is not initialized")
	}
	return nil
}

// commit saves the tasks and reports the change; the caller must hold tl.mu.
// The snapshot handed to onChange is the redacted one that was persisted, so
// journal and disk stay free of credentials.
func (tl *TaskList) commit(tasks []Task) error {
	saved, err := tl.store.Save(tasks)
	if err != nil {
		return err
	}
	if tl.onChange != nil {
		if err := tl.onChange(saved); err != nil {
			return fmt.Errorf("task change callback: %w", err)
		}
	}
	return nil
}

// dropDanglingRefs removes Blocks/BlockedBy entries that no longer point at
// an existing task, so deletions never leave stale dependencies behind.
func dropDanglingRefs(tasks []Task) []Task {
	known := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		known[task.ID] = true
	}
	for i := range tasks {
		tasks[i].Blocks = filterKnown(tasks[i].Blocks, known)
		tasks[i].BlockedBy = filterKnown(tasks[i].BlockedBy, known)
	}
	return tasks
}

// filterKnown keeps only the entries present in known.
func filterKnown(ids []string, known map[string]bool) []string {
	if len(ids) == 0 {
		return nil
	}
	kept := ids[:0:0]
	for _, id := range ids {
		if known[id] {
			kept = append(kept, id)
		}
	}
	return kept
}

// appendUnique appends entries that are not present yet.
func appendUnique(existing, entries []string) []string {
	seen := make(map[string]bool, len(existing)+len(entries))
	for _, id := range existing {
		seen[id] = true
	}
	for _, id := range entries {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		existing = append(existing, id)
	}
	return existing
}

func copyMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	copied := make(map[string]string, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
