package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A dependent can advance only after the prerequisite completion is durable.
// Race both service mutations to exercise either lock ordering and then check
// that the published and replayed graph agree about the outcome.
func TestTeamTaskDependencyCompletionRacesDependentAdvance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "dependency-completion-race")
	team, err := service.CreateTeam(context.Background(), request, "dependency-race")
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "prepare"})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := service.CreateTeamTask(context.Background(), request, team.ID, teams.Task{Title: "finish", BlockedBy: []string{prerequisite.ID}})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	complete := teams.TaskCompleted
	inProgress := teams.TaskInProgress
	for _, op := range []struct {
		id    string
		rev   uint64
		patch teams.TaskPatch
	}{
		{prerequisite.ID, prerequisite.Revision, teams.TaskPatch{Status: &complete}},
		{dependent.ID, dependent.Revision, teams.TaskPatch{Status: &inProgress}},
	} {
		op := op
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, updateErr := service.UpdateTeamTask(context.Background(), request, team.ID, op.id, op.rev, op.patch)
			results <- result{id: op.id, err: updateErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var dependencyError, advanceError error
	for result := range results {
		if result.id == prerequisite.ID {
			dependencyError = result.err
		} else {
			advanceError = result.err
		}
	}
	if dependencyError != nil {
		t.Fatalf("prerequisite completion failed: %v", dependencyError)
	}
	if advanceError != nil && !strings.Contains(advanceError.Error(), "incomplete dependency") {
		t.Fatalf("dependent advance failed for unexpected reason: %v", advanceError)
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotPrerequisite := projection.Tasks[prerequisite.ID]
	gotDependent := projection.Tasks[dependent.ID]
	if gotPrerequisite.Status != teams.TaskCompleted || gotPrerequisite.Revision != prerequisite.Revision+1 {
		t.Fatalf("prerequisite completion not durable: %+v", gotPrerequisite)
	}
	if advanceError == nil {
		if gotDependent.Status != teams.TaskInProgress || gotDependent.Revision != dependent.Revision+1 {
			t.Fatalf("accepted dependent advance diverged from completed prerequisite: %+v", gotDependent)
		}
	} else if gotDependent.Status != teams.TaskPending || gotDependent.Revision != dependent.Revision {
		t.Fatalf("rejected dependent advance changed durable task: %+v", gotDependent)
	}

	graph, err := teamTaskGraph(projection, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, ok := graph.Get(dependent.ID)
	wantViewStatus := teams.TaskInProgress
	if advanceError != nil {
		wantViewStatus = teams.TaskPending
	}
	if !ok || view.Status != wantViewStatus {
		t.Fatalf("dependent graph view=%+v found=%v advanceErr=%v", view, ok, advanceError)
	}
	if len(view.BlockedBy) != 1 || view.BlockedBy[0] != prerequisite.ID {
		t.Fatalf("dependency edge changed during concurrent completion: %+v", view)
	}
}
