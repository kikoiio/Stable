package teams

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newDomainTask(id string, dependencies ...string) Task {
	return Task{ID: id, TeamID: "team-1", Title: "Investigate " + id, BlockedBy: dependencies}
}

func requireDomainTask(t *testing.T, g *TaskGraph, task Task) Task {
	t.Helper()
	got, err := g.Create(task, Actor{Lead: true})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTaskGraphCanonicalDependenciesAndCompletionGate(t *testing.T) {
	g := NewTaskGraph("team-1")
	a := requireDomainTask(t, g, newDomainTask("a"))
	b := requireDomainTask(t, g, newDomainTask("b", "a", "a"))
	if b.Status != TaskBlocked || !reflect.DeepEqual(b.BlockedBy, []string{"a"}) {
		t.Fatalf("blocked view=%+v", b)
	}
	a, _ = g.Get("a")
	if !reflect.DeepEqual(a.Blocks, []string{"b"}) {
		t.Fatalf("reverse dependency=%+v", a)
	}
	canonical, _ := g.GetCanonical("b")
	if canonical.Status != TaskPending || len(canonical.Blocks) != 0 {
		t.Fatalf("derived state persisted in canonical view: %+v", canonical)
	}
	inProgress := TaskInProgress
	completed := TaskCompleted
	for _, status := range []TaskStatus{inProgress, completed} {
		if _, err := g.Update("b", b.Revision, TaskPatch{Status: &status}, Actor{Lead: true}); err == nil {
			t.Fatal("blocked task advanced before dependency completed")
		}
	}
	a, err := g.Update("a", a.Revision, TaskPatch{Status: &completed}, Actor{Lead: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = g.Get("b")
	if b.Status != TaskPending || b.Revision != 1 {
		t.Fatalf("derived unblocking must not invent a revision: %+v", b)
	}
	b, err = g.Update("b", b.Revision, TaskPatch{Status: &inProgress}, Actor{Lead: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Update("b", b.Revision, TaskPatch{Status: &completed}, Actor{Lead: true}); err != nil {
		t.Fatal(err)
	}
	pending := TaskPending
	if _, err = g.Update("a", a.Revision, TaskPatch{Status: &pending}, Actor{Lead: true}); err == nil {
		t.Fatal("reopening prerequisite left completed dependent blocked")
	}
}

func TestTaskGraphRejectsCyclesUnknownAndCrossTeamDependencies(t *testing.T) {
	g := NewTaskGraph("team-1")
	a := requireDomainTask(t, g, newDomainTask("a"))
	requireDomainTask(t, g, newDomainTask("b", "a"))
	cycle := []string{"b"}
	if _, err := g.Update("a", a.Revision, TaskPatch{BlockedBy: &cycle}, Actor{Lead: true}); !errors.Is(err, ErrDependency) {
		t.Fatalf("cycle accepted: %v", err)
	}
	for _, task := range []Task{newDomainTask("self", "self"), newDomainTask("missing", "unknown"), {ID: "other", TeamID: "team-2", Title: "foreign"}} {
		if _, err := g.Create(task, Actor{Lead: true}); err == nil {
			t.Fatalf("invalid dependency or ownership accepted: %+v", task)
		}
	}
	if after, _ := g.Get("a"); after.Revision != a.Revision || len(after.BlockedBy) != 0 {
		t.Fatal("rejected cycle mutated domain facts")
	}
}

func TestTaskGraphConcurrentClaimAndOwnerPermissions(t *testing.T) {
	g := NewTaskGraph("team-1")
	task := requireDomainTask(t, g, newDomainTask("claim"))
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"member-a", "member-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := g.Update(task.ID, task.Revision, TaskPatch{Assignee: &id}, Actor{MemberID: id})
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("claim results success=%d conflict=%d", success, conflict)
	}
	current, _ := g.Get(task.ID)
	other := "member-a"
	if other == current.Assignee {
		other = "member-b"
	}
	description := "Changed by foreign member"
	if _, err := g.Update(task.ID, current.Revision, TaskPatch{Description: &description}, Actor{MemberID: other}); !errors.Is(err, ErrPermission) {
		t.Fatalf("other owner mutated task: %v", err)
	}
	if _, err := g.Update(task.ID, current.Revision, TaskPatch{Assignee: &other}, Actor{MemberID: current.Assignee}); !errors.Is(err, ErrPermission) {
		t.Fatalf("member reassigned own task to peer: %v", err)
	}
	if _, err := g.Update(task.ID, current.Revision, TaskPatch{Assignee: &other}, Actor{Lead: true}); err != nil {
		t.Fatalf("lead could not correct owner: %v", err)
	}
}

func TestTaskGraphEventFirstCloneAndSnapshotRecovery(t *testing.T) {
	g := NewTaskGraph("team-1")
	a := requireDomainTask(t, g, newDomainTask("a"))
	requireDomainTask(t, g, newDomainTask("b", "a"))
	prospective := g.Clone()
	title := "Prospective event"
	if _, err := prospective.Update("a", a.Revision, TaskPatch{Title: &title}, Actor{Lead: true}); err != nil {
		t.Fatal(err)
	}
	if before, _ := g.Get("a"); before.Title == title {
		t.Fatal("prospective validation changed published projection")
	}
	ca, _ := g.GetCanonical("a")
	cb, _ := g.GetCanonical("b")
	restored, err := LoadTaskGraph("team-1", []Task{cb, ca})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.List(), g.List()) {
		t.Fatalf("restored graph differs: %+v vs %+v", restored.List(), g.List())
	}
	got, _ := restored.Get("b")
	got.BlockedBy[0] = "foreign"
	if original, _ := restored.Get("b"); original.BlockedBy[0] != "a" {
		t.Fatal("query shares mutable graph slice")
	}
	if _, err := LoadTaskGraph("team-1", []Task{ca, ca}); err == nil {
		t.Fatal("duplicate snapshot accepted")
	}
	if _, err := LoadTaskGraph("team-1", []Task{cb}); err == nil {
		t.Fatal("snapshot lost prerequisite accepted")
	}
}

func TestTaskGraphLimitsAndExplicitBlockedStatus(t *testing.T) {
	g := NewTaskGraph("team-1")
	for i := 0; i < MaxTeamTasks; i++ {
		requireDomainTask(t, g, newDomainTask(fmt.Sprintf("task-%03d", i)))
	}
	if _, err := g.Create(newDomainTask("overflow"), Actor{Lead: true}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("task count limit: %v", err)
	}
	for _, patch := range []TaskPatch{
		{Title: ptrDomain(strings.Repeat("x", MaxTaskTitleBytes+1))},
		{Description: ptrDomain(strings.Repeat("x", MaxTaskDescriptionBytes+1))},
		{Status: ptrDomain(TaskBlocked)},
	} {
		if _, err := g.Update("task-000", 1, patch, Actor{Lead: true}); err == nil {
			t.Fatal("invalid oversized/derived patch accepted")
		}
	}
	dependencies := make([]string, MaxTaskDependencies+1)
	for i := range dependencies {
		dependencies[i] = fmt.Sprintf("task-%03d", i+1)
	}
	if _, err := g.Update("task-000", 1, TaskPatch{BlockedBy: &dependencies}, Actor{Lead: true}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("dependency count limit: %v", err)
	}
	if _, err := g.Update("task-000", 0, TaskPatch{}, Actor{Lead: true}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("missing expected_revision accepted: %v", err)
	}
}

func ptrDomain[T any](value T) *T { return &value }
