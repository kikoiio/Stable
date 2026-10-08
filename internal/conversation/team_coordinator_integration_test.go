package conversation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
)

type coordinatorRunCapture struct {
	request         agent.ExecutionRequest
	effectiveSchema []llm.ToolSchema
	events          chan agent.ExecutionEvent
	done            chan agent.RunOutcome
	finishOnce      sync.Once
}

func (c *coordinatorRunCapture) finish(status agent.RunStatus) {
	c.finishOnce.Do(func() {
		close(c.events)
		c.done <- agent.RunOutcome{RunID: c.request.RunID, Status: status}
		close(c.done)
	})
}

type coordinatorRecordingRunner struct {
	defaults []llm.ToolSchema
	started  chan *coordinatorRunCapture
}

func (r *coordinatorRecordingRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	effective := request.ToolSchemas
	if effective == nil {
		effective = r.defaults
	}
	capture := &coordinatorRunCapture{
		request: request, effectiveSchema: append([]llm.ToolSchema(nil), effective...),
		events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1),
	}
	r.started <- capture
	return &agent.RunHandle{Events: capture.events, Done: capture.done}, nil
}

func (*coordinatorRecordingRunner) Cancel(string) error { return nil }

func TestCoordinatorModeIsSnapshottedForEachRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, fixtureRequest := teamServiceFixture(t, root, "fixture-parent")
	sessionID := fixtureRequest.Work.SessionID
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	defaults := []llm.ToolSchema{
		{Name: "read_file"}, {Name: "write_file"}, {Name: "command"},
		{Name: "team_list"}, {Name: "team_send"}, {Name: "team_task_update"},
	}
	runner := &coordinatorRecordingRunner{defaults: defaults, started: make(chan *coordinatorRunCapture, 2)}
	service.deps.Runner = runner
	service.deps.ToolSchemas = defaults
	updates := make(chan ServerMsg, 8)
	service.clients = map[chan ServerMsg]*clientSubscription{updates: {ch: updates}}
	var runA, runB *coordinatorRunCapture
	t.Cleanup(func() {
		if runA != nil {
			runA.finish(agent.RunCompleted)
		}
		if runB != nil {
			runB.finish(agent.RunCompleted)
		}
		for {
			select {
			case capture := <-runner.started:
				capture.finish(agent.RunCompleted)
			default:
				return
			}
		}
	})

	if _, err := service.handleTeamRequest(t.Context(), ClientMsg{Op: "team_coordinator", SessionID: sessionID, CoordinatorOn: true}); err != nil {
		t.Fatal(err)
	}
	requestA := agent.ExecutionRequest{
		RunID: "coordinator-a", Work: fixtureRequest.Work, Intent: "coordinate the review",
		Messages: []llm.Message{{Role: "user", Content: "Start run A."}},
	}
	if err := service.startRun(t.Context(), ClientMsg{SessionID: sessionID, Run: &requestA}, updates); err != nil {
		t.Fatal(err)
	}
	runA = receiveCoordinatorRun(t, runner.started)
	if !runA.request.TeamCoordinator {
		t.Fatal("run A did not receive the trusted coordinator mode")
	}
	if got, want := schemaNames(runA.request.ToolSchemas), []string{"team_list", "team_send", "team_task_update"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run A request schemas=%v, want %v", got, want)
	}
	if got, want := schemaNames(runA.effectiveSchema), []string{"team_list", "team_send", "team_task_update"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run A effective schemas=%v, want %v", got, want)
	}
	if len(runA.request.Messages) == 0 || runA.request.Messages[0].Role != "system" || !strings.Contains(runA.request.Messages[0].Content, "团队协调器模式") {
		t.Fatalf("run A lacks coordinator prompt guidance: %+v", runA.request.Messages)
	}

	if _, err := service.handleTeamRequest(t.Context(), ClientMsg{Op: "team_coordinator", SessionID: sessionID, CoordinatorOn: false}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	activeA := service.activeRequests[requestA.RunID]
	service.mu.Unlock()
	if !activeA.TeamCoordinator || !reflect.DeepEqual(schemaNames(activeA.ToolSchemas), []string{"team_list", "team_send", "team_task_update"}) {
		t.Fatalf("changing the session mode mutated active run A: coordinator=%v schemas=%v", activeA.TeamCoordinator, schemaNames(activeA.ToolSchemas))
	}

	requestB := agent.ExecutionRequest{
		RunID: "coordinator-b", Work: fixtureRequest.Work, Intent: "continue normally",
		Messages: []llm.Message{{Role: "user", Content: "Start run B."}},
	}
	if err := service.startRun(t.Context(), ClientMsg{SessionID: sessionID, Run: &requestB}, updates); err != nil {
		t.Fatal(err)
	}
	runB = receiveCoordinatorRun(t, runner.started)
	if runB.request.TeamCoordinator {
		t.Fatal("run B inherited coordinator mode after it was disabled")
	}
	if runB.request.ToolSchemas != nil {
		t.Fatalf("ordinary run B unexpectedly received an explicit restricted schema set: %v", schemaNames(runB.request.ToolSchemas))
	}
	if got, want := schemaNames(runB.effectiveSchema), schemaNames(defaults); !reflect.DeepEqual(got, want) {
		t.Fatalf("run B effective schemas=%v, want ordinary defaults %v", got, want)
	}
	if len(runB.request.Messages) == 0 || runB.request.Messages[0].Role != "system" || strings.Contains(runB.request.Messages[0].Content, "团队协调器模式") {
		t.Fatalf("run B retained coordinator prompt guidance: %+v", runB.request.Messages)
	}
	service.mu.Lock()
	activeA = service.activeRequests[requestA.RunID]
	activeB := service.activeRequests[requestB.RunID]
	service.mu.Unlock()
	if !activeA.TeamCoordinator || activeB.TeamCoordinator {
		t.Fatalf("active run modes were not static: A=%v B=%v", activeA.TeamCoordinator, activeB.TeamCoordinator)
	}

	runA.finish(agent.RunCompleted)
	runB.finish(agent.RunCompleted)
}

func receiveCoordinatorRun(t *testing.T, started <-chan *coordinatorRunCapture) *coordinatorRunCapture {
	t.Helper()
	select {
	case capture := <-started:
		return capture
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not receive the run")
		return nil
	}
}

func schemaNames(schemas []llm.ToolSchema) []string {
	names := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		names = append(names, schema.Name)
	}
	return names
}
