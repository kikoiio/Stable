package teams

import (
	"errors"
	"sort"
	"sync"
)

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskCompleted  TaskStatus = "completed"
	TaskBlocked    TaskStatus = "blocked"
)

type Task struct {
	ID          string     `json:"id"`
	TeamID      string     `json:"team_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      TaskStatus `json:"status"`
	Assignee    string     `json:"assignee,omitempty"`
	BlockedBy   []string   `json:"blocked_by"`
	Blocks      []string   `json:"blocks"`
	Revision    uint64     `json:"revision"`
	CreatedBy   string     `json:"created_by"`
}

// Nil fields preserve existing values. BlockedBy is the entire canonical set,
// not a caller-controlled Blocks mirror. Revision is supplied separately.
type TaskPatch struct {
	Title       *string
	Description *string
	Status      *TaskStatus
	Assignee    *string
	BlockedBy   *[]string
}

// TaskGraph is a bounded in-memory domain view, never a persistence authority.
// Service must serialize durable writes under its event lock. This mutex also
// makes prospective domain CAS checks safe for independent callers/tests.
type TaskGraph struct {
	mu     sync.Mutex
	teamID string
	tasks  map[string]Task
}

func NewTaskGraph(teamID string) *TaskGraph {
	return &TaskGraph{teamID: teamID, tasks: map[string]Task{}}
}

// LoadTaskGraph reconstructs canonical snapshots from durable facts. Derived
// blocked statuses/Blocks are not accepted as a second source of truth.
func LoadTaskGraph(teamID string, tasks []Task) (*TaskGraph, error) {
	g := NewTaskGraph(teamID)
	if ValidateID(teamID) != nil {
		return nil, ErrPermission
	}
	if len(tasks) > MaxTeamTasks {
		return nil, ErrCapacity
	}
	for _, task := range tasks {
		if task.TeamID != teamID {
			return nil, ErrPermission
		}
		if _, ok := g.tasks[task.ID]; ok {
			return nil, errors.New("duplicate task in domain snapshot")
		}
		if err := validateTask(task); err != nil {
			return nil, err
		}
		if task.Revision == 0 {
			return nil, errors.New("task snapshot requires a revision")
		}
		task.BlockedBy = canonicalIDs(task.BlockedBy)
		task.Blocks = nil
		g.tasks[task.ID] = copyTask(task)
	}
	for _, task := range g.tasks {
		if err := g.validateGraph(task); err != nil {
			return nil, err
		}
		break
	}
	return g, nil
}

// Clone permits event-first callers to validate an operation prospectively,
// then replace their projection only after persistence has succeeded.
func (g *TaskGraph) Clone() *TaskGraph {
	g.mu.Lock()
	defer g.mu.Unlock()
	copy := NewTaskGraph(g.teamID)
	for id, task := range g.tasks {
		copy.tasks[id] = copyTask(task)
	}
	return copy
}

func (g *TaskGraph) GetCanonical(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	task, ok := g.tasks[id]
	if !ok {
		return Task{}, false
	}
	return copyTask(task), true
}

func (g *TaskGraph) Create(task Task, actor Actor) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := actor.Validate(); err != nil {
		return Task{}, err
	}
	if ValidateID(g.teamID) != nil || task.TeamID != g.teamID {
		return Task{}, ErrPermission
	}
	if len(g.tasks) >= MaxTeamTasks {
		return Task{}, ErrCapacity
	}
	if _, exists := g.tasks[task.ID]; exists {
		return Task{}, errors.New("team task ID already exists")
	}
	if !actor.Lead && task.Assignee != "" && task.Assignee != actor.MemberID {
		return Task{}, ErrPermission
	}
	if task.Status == "" {
		task.Status = TaskPending
	}
	if task.Status != TaskPending {
		return Task{}, errors.New("new team tasks must be pending")
	}
	if err := validateTask(task); err != nil {
		return Task{}, err
	}
	task.BlockedBy = canonicalIDs(task.BlockedBy)
	task.Blocks = nil
	task.Revision = 1
	task.CreatedBy = actor.MemberID
	if actor.Lead {
		task.CreatedBy = Lead
	}
	if err := g.validateGraph(task); err != nil {
		return Task{}, err
	}
	g.tasks[task.ID] = copyTask(task)
	return g.project(task), nil
}

func (g *TaskGraph) Update(id string, expectedRevision uint64, patch TaskPatch, actor Actor) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := actor.Validate(); err != nil {
		return Task{}, err
	}
	current, exists := g.tasks[id]
	if !exists {
		return Task{}, ErrNotFound
	}
	if expectedRevision == 0 || current.Revision != expectedRevision {
		return g.project(current), ErrRevisionConflict
	}
	if !actor.Lead {
		claim := current.Assignee == "" && patch.Assignee != nil && *patch.Assignee == actor.MemberID
		if current.Assignee != actor.MemberID && !claim {
			return Task{}, ErrPermission
		}
		if patch.Assignee != nil && *patch.Assignee != actor.MemberID {
			return Task{}, ErrPermission
		}
	}
	next := copyTask(current)
	if patch.Title != nil {
		next.Title = *patch.Title
	}
	if patch.Description != nil {
		next.Description = *patch.Description
	}
	if patch.Assignee != nil {
		next.Assignee = *patch.Assignee
	}
	if patch.Status != nil {
		next.Status = *patch.Status
	}
	if patch.BlockedBy != nil {
		next.BlockedBy = canonicalIDs(*patch.BlockedBy)
	}
	if err := validateTask(next); err != nil {
		return Task{}, err
	}
	if err := g.validateGraph(next); err != nil {
		return Task{}, err
	}
	if next.Revision == ^uint64(0) {
		return Task{}, errors.New("task revision exhausted")
	}
	next.Revision++
	g.tasks[id] = next
	return g.project(next), nil
}

func (g *TaskGraph) Get(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok {
		return Task{}, false
	}
	return g.project(t), true
}

func (g *TaskGraph) List() []Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Task, 0, len(g.tasks))
	for _, task := range g.tasks {
		out = append(out, g.project(task))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func validateTask(t Task) error {
	if ValidateID(t.ID) != nil || ValidateID(t.TeamID) != nil {
		return errors.New("invalid team task identity")
	}
	if ValidateText(t.Title, MaxTaskTitleBytes, true) != nil || ValidateText(t.Description, MaxTaskDescriptionBytes, false) != nil {
		return errors.New("invalid team task title or description")
	}
	if t.Assignee != "" && (t.Assignee == Lead || ValidateID(t.Assignee) != nil) {
		return errors.New("task assignee must be a member ID")
	}
	switch t.Status {
	case TaskPending, TaskInProgress, TaskCompleted:
	case TaskBlocked:
		return errors.New("blocked task status is derived from dependencies")
	default:
		return errors.New("invalid team task status")
	}
	if len(t.BlockedBy) > MaxTaskDependencies {
		return ErrCapacity
	}
	for _, id := range t.BlockedBy {
		if ValidateID(id) != nil || id == t.ID {
			return ErrDependency
		}
	}
	return nil
}

// Validate the complete prospective graph so reopening a prerequisite cannot
// leave an already completed/in-progress dependent behind an unresolved edge.
func (g *TaskGraph) validateGraph(next Task) error {
	view := make(map[string]Task, len(g.tasks)+1)
	for id, t := range g.tasks {
		view[id] = t
	}
	view[next.ID] = next
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(id string) error {
		if visiting[id] {
			return ErrDependency
		}
		if visited[id] {
			return nil
		}
		task, ok := view[id]
		if !ok || task.TeamID != g.teamID {
			return ErrDependency
		}
		visiting[id] = true
		for _, dependency := range task.BlockedBy {
			prerequisite, ok := view[dependency]
			if !ok || prerequisite.TeamID != g.teamID {
				return ErrDependency
			}
			if (task.Status == TaskInProgress || task.Status == TaskCompleted) && prerequisite.Status != TaskCompleted {
				return errors.New("task has an incomplete dependency")
			}
			if err := walk(dependency); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range view {
		if err := walk(id); err != nil {
			return err
		}
	}
	return nil
}

func (g *TaskGraph) project(task Task) Task {
	task = copyTask(task)
	task.Blocks = make([]string, 0)
	for _, candidate := range g.tasks {
		for _, id := range candidate.BlockedBy {
			if id == task.ID {
				task.Blocks = append(task.Blocks, candidate.ID)
				break
			}
		}
	}
	sort.Strings(task.Blocks)
	if task.Status == TaskPending {
		for _, id := range task.BlockedBy {
			if g.tasks[id].Status != TaskCompleted {
				task.Status = TaskBlocked
				break
			}
		}
	}
	return task
}

func copyTask(task Task) Task {
	task.BlockedBy = append(make([]string, 0, len(task.BlockedBy)), task.BlockedBy...)
	task.Blocks = append(make([]string, 0, len(task.Blocks)), task.Blocks...)
	return task
}

func canonicalIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}
