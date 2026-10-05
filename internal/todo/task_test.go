package todo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestList(t *testing.T) (*TaskList, string) {
	t.Helper()
	root := t.TempDir()
	return NewTaskList(root, "session-a", nil), root
}

func mustCreate(t *testing.T, tl *TaskList, subject string) Task {
	t.Helper()
	task, err := tl.Create(subject, "description of "+subject, "", nil)
	if err != nil {
		t.Fatalf("create %q: %v", subject, err)
	}
	return task
}

func TestCreateGetListRoundtrip(t *testing.T) {
	tl, root := newTestList(t)
	created, err := tl.Create("write tests", "cover the todo module", "Writing tests", map[string]string{"kind": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.ID, "task-") {
		t.Fatalf("task id %q lacks task- prefix", created.ID)
	}
	if created.Status != StatusPending {
		t.Fatalf("new task status %q", created.Status)
	}
	if created.Metadata["kind"] != "test" {
		t.Fatalf("metadata lost: %v", created.Metadata)
	}

	got, err := tl.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "write tests" || got.Description != "cover the todo module" || got.ActiveForm != "Writing tests" {
		t.Fatalf("get mismatch: %+v", got)
	}

	list, err := tl.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	// A fresh task list over the same session recovers the persisted tasks.
	reopened := NewTaskList(root, "session-a", nil)
	list, err = reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Subject != "write tests" {
		t.Fatalf("reopened list mismatch: %+v", list)
	}
}

func TestCreateRequiresSubject(t *testing.T) {
	tl, _ := newTestList(t)
	for _, subject := range []string{"", "   "} {
		if _, err := tl.Create(subject, "d", "", nil); err == nil {
			t.Fatalf("create accepted subject %q", subject)
		}
	}
	list, err := tl.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("failed creates persisted tasks: %v %v", list, err)
	}
}

func TestGetUnknownID(t *testing.T) {
	tl, _ := newTestList(t)
	if _, err := tl.Get("task-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown id: %v", err)
	}
}

func TestUpdateFieldsAndStatusTransitions(t *testing.T) {
	tl, _ := newTestList(t)
	task := mustCreate(t, tl, "step")

	newSubject := "renamed step"
	newDescription := "new description"
	newForm := "Renaming"
	newOwner := "agent-b"
	status := StatusInProgress
	updated, err := tl.Update(task.ID, UpdatePatch{
		Subject:     &newSubject,
		Description: &newDescription,
		ActiveForm:  &newForm,
		Owner:       &newOwner,
		Status:      &status,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subject != newSubject || updated.Description != newDescription ||
		updated.ActiveForm != newForm || updated.Owner != newOwner || updated.Status != StatusInProgress {
		t.Fatalf("update mismatch: %+v", updated)
	}

	// Forward migration in_progress -> completed.
	status = StatusCompleted
	if _, err = tl.Update(task.ID, UpdatePatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	// Backward setting is allowed.
	status = StatusPending
	if _, err = tl.Update(task.ID, UpdatePatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	got, err := tl.Get(task.ID)
	if err != nil || got.Status != StatusPending {
		t.Fatalf("backward status: %+v %v", got, err)
	}

	invalid := Status("archived")
	if _, err = tl.Update(task.ID, UpdatePatch{Status: &invalid}); err == nil {
		t.Fatal("invalid status accepted")
	}
	if _, err = tl.Update("task-missing", UpdatePatch{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown id: %v", err)
	}

	empty := ""
	if _, err = tl.Update(task.ID, UpdatePatch{Subject: &empty}); err == nil {
		t.Fatal("update to empty subject accepted")
	}
}

func TestUpdateDeleteRemovesAndCleansReferences(t *testing.T) {
	tl, _ := newTestList(t)
	blocker := mustCreate(t, tl, "blocker")
	middle := mustCreate(t, tl, "middle")
	blocked := mustCreate(t, tl, "blocked")

	if _, err := tl.Update(middle.ID, UpdatePatch{AddBlocks: []string{blocked.ID}, AddBlockedBy: []string{blocker.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Update(blocked.ID, UpdatePatch{AddBlockedBy: []string{middle.ID}}); err != nil {
		t.Fatal(err)
	}

	removed, err := tl.Update(middle.ID, UpdatePatch{Status: statusPtr(StatusDeleted)})
	if err != nil {
		t.Fatal(err)
	}
	if removed.ID != middle.ID {
		t.Fatalf("delete returned %+v", removed)
	}
	if _, err := tl.Get(middle.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted task still gettable: %v", err)
	}

	list, err := tl.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list after delete: %v %v", list, err)
	}
	for _, task := range list {
		if contains(task.Blocks, middle.ID) || contains(task.BlockedBy, middle.ID) {
			t.Fatalf("dangling reference to %s survived on %s", middle.ID, task.ID)
		}
	}
	got, _ := tl.Get(blocked.ID)
	if contains(got.BlockedBy, blocker.ID) {
		t.Fatalf("unrelated references were dropped: %+v", got)
	}
}

func TestUpdateDropsDanglingDependencyImmediately(t *testing.T) {
	tl, _ := newTestList(t)
	task := mustCreate(t, tl, "task")
	updated, err := tl.Update(task.ID, UpdatePatch{
		AddBlocks:    []string{"task-nonexistent"},
		AddBlockedBy: []string{"task-also-missing", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Blocks) != 0 || len(updated.BlockedBy) != 0 {
		t.Fatalf("dangling references kept: %+v", updated)
	}
}

func TestCreateLimitOf100Tasks(t *testing.T) {
	tl, _ := newTestList(t)
	for i := 0; i < MaxTasks; i++ {
		if _, err := tl.Create("task subject", "d", "", nil); err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
	}
	if _, err := tl.Create("one too many", "d", "", nil); err == nil {
		t.Fatal("101st create accepted")
	}
	list, err := tl.List()
	if err != nil || len(list) != MaxTasks {
		t.Fatalf("list size: %d %v", len(list), err)
	}
}

func TestConcurrentUpdatesDoNotLoseUpdates(t *testing.T) {
	tl, _ := newTestList(t)
	target := mustCreate(t, tl, "shared target")
	workers := 10
	own := make([]Task, workers)
	for i := range own {
		own[i] = mustCreate(t, tl, "worker task")
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := StatusInProgress
			if _, err := tl.Update(own[i].ID, UpdatePatch{Status: &status}); err != nil {
				errs[i] = err
				return
			}
			if _, err := tl.Update(target.ID, UpdatePatch{AddBlocks: []string{own[i].ID}}); err != nil {
				errs[i] = err
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}

	got, err := tl.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Blocks) != workers {
		t.Fatalf("lost updates: target blocks %v", got.Blocks)
	}
	seen := map[string]bool{}
	for _, id := range got.Blocks {
		if seen[id] {
			t.Fatalf("duplicated block entry %s", id)
		}
		seen[id] = true
	}
	for _, task := range own {
		got, err := tl.Get(task.ID)
		if err != nil || got.Status != StatusInProgress {
			t.Fatalf("worker task %s: %+v %v", task.ID, got, err)
		}
	}
}

func TestOnChangeErrorPropagates(t *testing.T) {
	root := t.TempDir()
	boom := errors.New("journal write failed")
	tl := NewTaskList(root, "session-a", func(tasks []Task) error {
		return boom
	})
	if _, err := tl.Create("subject", "d", "", nil); !errors.Is(err, boom) {
		t.Fatalf("create onChange error: %v", err)
	}
	plain := NewTaskList(root, "session-a", nil)
	seed, err := plain.Create("seed", "d", "", nil)
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	failing := NewTaskList(root, "session-a", func(tasks []Task) error { return boom })
	if _, err := failing.Update(seed.ID, UpdatePatch{AddBlocks: []string{seed.ID}}); !errors.Is(err, boom) {
		t.Fatalf("update onChange error: %v", err)
	}
	// Save happened before the callback failed, so the change persists.
	again := NewTaskList(root, "session-a", nil)
	got, err := again.Get(seed.ID)
	if err != nil || len(got.Blocks) != 1 {
		t.Fatalf("change not persisted before onChange failure: %+v %v", got, err)
	}
}

func TestOnChangeReceivesFullSnapshots(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	var sizes []int
	tl := NewTaskList(root, "session-a", func(tasks []Task) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(tasks))
		return nil
	})
	first := mustCreate(t, tl, "one")
	second := mustCreate(t, tl, "two")
	status := StatusCompleted
	if _, err := tl.Update(second.ID, UpdatePatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Update(first.ID, UpdatePatch{Status: statusPtr(StatusDeleted)}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 2, 1}
	if len(sizes) != len(want) {
		t.Fatalf("snapshot sizes %v want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("snapshot sizes %v want %v", sizes, want)
		}
	}
}

func TestUnsafeSessionIDsRejected(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "a/b", `a\b`, "..", "../escape", "a/../b"} {
		tl := NewTaskList(root, id, nil)
		if _, err := tl.Create("subject", "d", "", nil); err == nil {
			t.Fatalf("create accepted session id %q", id)
		}
		if _, err := tl.Get("task-x"); err == nil {
			t.Fatalf("get accepted session id %q", id)
		}
		if _, err := tl.List(); err == nil {
			t.Fatalf("list accepted session id %q", id)
		}
		if _, err := tl.Update("task-x", UpdatePatch{}); err == nil {
			t.Fatalf("update accepted session id %q", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".stable")); !os.IsNotExist(err) {
		t.Fatalf("unsafe session ids touched the state directory: %v", err)
	}
}

func TestTaskIDFormat(t *testing.T) {
	id := randomID()
	if !strings.HasPrefix(id, "task-") || len(id) != len("task-")+16 {
		t.Fatalf("unexpected id %q", id)
	}
	for _, r := range id[len("task-"):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("id %q is not lowercase hex", id)
		}
	}
}

func statusPtr(status Status) *Status { return &status }

func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
