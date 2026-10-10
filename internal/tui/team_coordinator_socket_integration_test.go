package tui

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
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// Coordinator selection and exit travel through the same TUI command and
// conversation socket used interactively. Mode changes apply only to the next
// run; an already active run keeps its original prompt and tool set.
func TestTeamTUICoordinatorModeEnableDisableAppliesToNextRunOnly(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "tuc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	defaultSchemas := []llm.ToolSchema{
		{Name: "read_file"}, {Name: "write_file"}, {Name: "command"},
		{Name: "team_list"}, {Name: "team_send"}, {Name: "team_task_update"},
	}
	runner := &tuiCoordinatorRecordingRunner{
		defaults: defaultSchemas,
		started:  make(chan *tuiCoordinatorRunCapture, 3),
	}
	socket := filepath.Join(root, "s")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: runner, Agents: agentcatalog.New("", ""), ToolSchemas: defaultSchemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID

	initialStream, initial := openTUITestCoordinatorRun(t, reqctx, socket, runner, sessionID, "initial ordinary run")
	defer initialStream.Close()
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, initial.request.RunID, true

	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create coordinator-socket")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted the team")
	}
	model, enableResult := submitAcceptanceTeamCommand(t, model, "/teams coordinator "+team.ID)
	enableResponse := acceptanceTeamResponse(t, enableResult, "team_coordinator")
	if !enableResponse.CoordinatorOn || enableResponse.CoordinatorTeamID != team.ID {
		t.Fatalf("TUI coordinator enable response=%+v", enableResponse)
	}
	if !model.Pending || model.ActiveRunID != initial.request.RunID {
		t.Fatalf("enabling coordinator changed active parent state: pending=%v run=%q", model.Pending, model.ActiveRunID)
	}

	finishTUITestCoordinatorRun(t, initialStream, initial)
	coordinatorStream, coordinator := openTUITestCoordinatorRun(t, reqctx, socket, runner, sessionID, "team coordinator run")
	defer coordinatorStream.Close()
	if !coordinator.request.TeamCoordinator || coordinator.request.TeamCoordinatorTeamID != team.ID {
		t.Fatalf("next run lost coordinator binding: enabled=%v team=%q, want team %q", coordinator.request.TeamCoordinator, coordinator.request.TeamCoordinatorTeamID, team.ID)
	}
	wantCoordinatorSchemas := []string{"team_list", "team_send", "team_task_update"}
	if got := tuiCoordinatorSchemaNames(coordinator.request.ToolSchemas); !reflect.DeepEqual(got, wantCoordinatorSchemas) {
		t.Fatalf("coordinator request schemas=%v, want %v", got, wantCoordinatorSchemas)
	}
	if got := tuiCoordinatorSchemaNames(coordinator.effectiveSchemas); !reflect.DeepEqual(got, wantCoordinatorSchemas) {
		t.Fatalf("coordinator effective schemas=%v, want %v", got, wantCoordinatorSchemas)
	}
	if !tuiCoordinatorPromptContains(coordinator.request.Messages, team.ID) {
		t.Fatalf("coordinator prompt omitted its team-only boundary: %+v", coordinator.request.Messages)
	}

	model.ActiveRunID, model.Pending = coordinator.request.RunID, true
	model, disableResult := submitAcceptanceTeamCommand(t, model, "/teams coordinator off")
	disableResponse := acceptanceTeamResponse(t, disableResult, "team_coordinator")
	if disableResponse.CoordinatorOn || disableResponse.CoordinatorTeamID != "" {
		t.Fatalf("TUI coordinator disable response=%+v", disableResponse)
	}
	if !model.Pending || model.ActiveRunID != coordinator.request.RunID {
		t.Fatalf("disabling coordinator changed active parent state: pending=%v run=%q", model.Pending, model.ActiveRunID)
	}
	if !coordinator.request.TeamCoordinator || coordinator.request.TeamCoordinatorTeamID != team.ID ||
		!reflect.DeepEqual(tuiCoordinatorSchemaNames(coordinator.request.ToolSchemas), wantCoordinatorSchemas) ||
		!tuiCoordinatorPromptContains(coordinator.request.Messages, team.ID) {
		t.Fatal("disabling coordinator mutated the already admitted run")
	}

	finishTUITestCoordinatorRun(t, coordinatorStream, coordinator)
	ordinaryStream, ordinary := openTUITestCoordinatorRun(t, reqctx, socket, runner, sessionID, "ordinary run after coordinator exit")
	defer ordinaryStream.Close()
	if ordinary.request.TeamCoordinator || ordinary.request.TeamCoordinatorTeamID != "" {
		t.Fatalf("run after coordinator exit retained team binding: enabled=%v team=%q", ordinary.request.TeamCoordinator, ordinary.request.TeamCoordinatorTeamID)
	}
	if len(ordinary.request.ToolSchemas) != 0 {
		t.Fatalf("ordinary run unexpectedly carried a restricted schema override: %v", tuiCoordinatorSchemaNames(ordinary.request.ToolSchemas))
	}
	if got, want := tuiCoordinatorSchemaNames(ordinary.effectiveSchemas), tuiCoordinatorSchemaNames(defaultSchemas); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary effective schemas=%v, want defaults %v", got, want)
	}
	if tuiCoordinatorPromptContains(ordinary.request.Messages, team.ID) {
		t.Fatalf("ordinary prompt retained coordinator instructions: %+v", ordinary.request.Messages)
	}
	if !containsTUIString(tuiCoordinatorSchemaNames(ordinary.effectiveSchemas), "read_file") {
		t.Fatal("ordinary run did not regain the normal project tools")
	}
	finishTUITestCoordinatorRun(t, ordinaryStream, ordinary)
}

func openTUITestCoordinatorRun(t *testing.T, ctx context.Context, socket string, runner *tuiCoordinatorRecordingRunner, sessionID, intent string) (*conversation.StreamClient, *tuiCoordinatorRunCapture) {
	t.Helper()
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conversation.OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID:  runID,
		Work:   agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Model:  "fixture-model",
		Intent: intent, Messages: []llm.Message{{Role: "user", Content: intent}},
	})
	if err != nil {
		t.Fatalf("open %q: %v", intent, err)
	}
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		_ = stream.Close()
		t.Fatalf("start %q: message=%+v err=%v", intent, started, err)
	}
	select {
	case capture := <-runner.started:
		if capture.request.RunID != runID {
			t.Fatalf("runner started run %q, want %q", capture.request.RunID, runID)
		}
		return stream, capture
	case <-ctx.Done():
		_ = stream.Close()
		t.Fatalf("wait for runner to start %q: %v", intent, ctx.Err())
		return nil, nil
	case <-time.After(5 * time.Second):
		_ = stream.Close()
		t.Fatalf("runner did not start %q", intent)
		return nil, nil
	}
}

func finishTUITestCoordinatorRun(t *testing.T, stream *conversation.StreamClient, capture *tuiCoordinatorRunCapture) {
	t.Helper()
	capture.finish(agent.RunCompleted)
	for {
		message, err := stream.Receive()
		if err != nil {
			t.Fatalf("wait for run %q completion: %v", capture.request.RunID, err)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				t.Fatalf("run %q outcome=%+v, want completed", capture.request.RunID, message.Outcome)
			}
			return
		}
	}
}

type tuiCoordinatorRecordingRunner struct {
	defaults []llm.ToolSchema
	started  chan *tuiCoordinatorRunCapture
}

func (r *tuiCoordinatorRecordingRunner) Start(_ context.Context, request agent.ExecutionRequest) (*agent.RunHandle, error) {
	effective := request.ToolSchemas
	if effective == nil {
		effective = r.defaults
	}
	capture := &tuiCoordinatorRunCapture{
		request: request, effectiveSchemas: append([]llm.ToolSchema(nil), effective...),
		events: make(chan agent.ExecutionEvent), done: make(chan agent.RunOutcome, 1),
	}
	r.started <- capture
	return &agent.RunHandle{Events: capture.events, Done: capture.done}, nil
}

func (*tuiCoordinatorRecordingRunner) Cancel(string) error { return nil }

type tuiCoordinatorRunCapture struct {
	request          agent.ExecutionRequest
	effectiveSchemas []llm.ToolSchema
	events           chan agent.ExecutionEvent
	done             chan agent.RunOutcome
	once             sync.Once
}

func (r *tuiCoordinatorRunCapture) finish(status agent.RunStatus) {
	r.once.Do(func() {
		close(r.events)
		r.done <- agent.RunOutcome{RunID: r.request.RunID, Status: status}
		close(r.done)
	})
}

func tuiCoordinatorSchemaNames(schemas []llm.ToolSchema) []string {
	names := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		names = append(names, schema.Name)
	}
	return names
}

func tuiCoordinatorPromptContains(messages []llm.Message, teamID string) bool {
	for _, message := range messages {
		if message.Role == "system" && strings.Contains(message.Content, "团队协调器模式") && strings.Contains(message.Content, teamID) {
			return true
		}
	}
	return false
}

func containsTUIString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
