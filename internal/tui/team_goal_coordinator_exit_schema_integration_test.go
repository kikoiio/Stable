package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestGoalCoordinatorExitRestoresNormalToolsAndPromptForNextRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root, err := os.MkdirTemp(os.TempDir(), "m09-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project := filepath.Join(root, "project")
	goalRoot := filepath.Join(root, "goal-root")
	for _, dir := range []string{project, goalRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
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
	runner := &tuiCoordinatorRecordingRunner{defaults: defaultSchemas, started: make(chan *tuiCoordinatorRunCapture, 3)}
	socket := filepath.Join(root, "s")
	service, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: runner, Agents: agentcatalog.New("", ""), ToolSchemas: defaultSchemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	created, err := conversation.Request(ctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	const goalID = "goal-coordinator-exit-schema"
	if _, err := db.CreateGoal(ctx, core.Goal{
		ID: goalID, Objective: "coordinate one scoped work item", AllowedRoot: goalRoot, SourceSessionID: sessionID,
	}); err != nil {
		t.Fatal(err)
	}
	work := agent.WorkRef{Kind: agent.WorkGoal, SessionID: sessionID, GoalID: goalID, WorkItemID: "item-coordinator-exit"}

	initialStream, initial := openTUITestGoalCoordinatorRun(t, ctx, socket, runner, work, "prepare the scoped team", "")
	defer initialStream.Close()
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, initial.request.RunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create goal-coordinator-exit")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil || team.Scope.GoalID != goalID || team.Scope.WorkItemID != work.WorkItemID {
		t.Fatalf("TUI created wrong Goal team scope: %+v", team)
	}
	finishTUITestCoordinatorRun(t, initialStream, initial)

	coordinatorStream, coordinator := openTUITestGoalCoordinatorRun(t, ctx, socket, runner, work, "coordinate the authorized Goal team", team.ID)
	defer coordinatorStream.Close()
	wantCoordinatorSchemas := []string{"team_list", "team_send", "team_task_update"}
	if !coordinator.request.TeamCoordinator || coordinator.request.TeamCoordinatorTeamID != team.ID {
		t.Fatalf("Goal coordinator binding enabled=%v team=%q, want team %q", coordinator.request.TeamCoordinator, coordinator.request.TeamCoordinatorTeamID, team.ID)
	}
	if got := tuiCoordinatorSchemaNames(coordinator.request.ToolSchemas); !reflect.DeepEqual(got, wantCoordinatorSchemas) {
		t.Fatalf("Goal coordinator schemas=%v, want %v", got, wantCoordinatorSchemas)
	}
	if !tuiCoordinatorPromptContains(coordinator.request.Messages, team.ID) {
		t.Fatalf("Goal coordinator prompt omitted its team-only instruction: %+v", coordinator.request.Messages)
	}
	finishTUITestCoordinatorRun(t, coordinatorStream, coordinator)

	ordinaryStream, ordinary := openTUITestGoalCoordinatorRun(t, ctx, socket, runner, work, "continue ordinary Goal work", "")
	defer ordinaryStream.Close()
	if ordinary.request.TeamCoordinator || ordinary.request.TeamCoordinatorTeamID != "" {
		t.Fatalf("ordinary Goal run inherited coordinator mode: enabled=%v team=%q", ordinary.request.TeamCoordinator, ordinary.request.TeamCoordinatorTeamID)
	}
	if len(ordinary.request.ToolSchemas) != 0 {
		t.Fatalf("ordinary Goal run retained a restricted schema override: %v", tuiCoordinatorSchemaNames(ordinary.request.ToolSchemas))
	}
	if got, want := tuiCoordinatorSchemaNames(ordinary.effectiveSchemas), tuiCoordinatorSchemaNames(defaultSchemas); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary Goal effective schemas=%v, want defaults %v", got, want)
	}
	for _, name := range []string{"read_file", "write_file", "command", "team_list", "team_send", "team_task_update"} {
		if !containsTUIString(tuiCoordinatorSchemaNames(ordinary.effectiveSchemas), name) {
			t.Fatalf("ordinary Goal tools did not restore %q: %v", name, tuiCoordinatorSchemaNames(ordinary.effectiveSchemas))
		}
	}
	for _, message := range ordinary.request.Messages {
		if strings.Contains(message.Content, "团队协调器模式") || strings.Contains(message.Content, team.ID) {
			t.Fatalf("ordinary Goal prompt retained coordinator instructions or team binding: %+v", message)
		}
	}
	if len(ordinary.request.Messages) == 0 || ordinary.request.Messages[len(ordinary.request.Messages)-1].Content != "continue ordinary Goal work" {
		t.Fatalf("ordinary Goal prompt did not preserve the current work input: %+v", ordinary.request.Messages)
	}

	// Exercise both a project tool and a team-only tool through the real
	// executor using the exact restored Goal request captured from the socket.
	readSandbox := &goalCoordinatorRestoreSandbox{output: "fixture file content after coordinator exit"}
	teamHost := &agent.TeamToolHost{}
	teamHost.Bind(func(callCtx context.Context, request agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
		return service.ExecuteTeamTool(callCtx, request, call)
	})
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: goalCoordinatorRestoreGate{}, Sandbox: readSandbox, HelperPath: helperPath,
	}, execution.WithTeamToolHost(teamHost)).ForRun(ordinary.request)
	if err != nil {
		t.Fatalf("construct ordinary Goal executor: %v", err)
	}
	read, err := executor.Execute(ctx, llm.ToolUse{
		ID: "restored-goal-read-file", Name: "read_file", Arguments: []byte(`{"file_path":"fixture.txt"}`),
	})
	if err != nil || read.Status != agent.ToolSucceeded || read.IsError || read.Content != readSandbox.output {
		t.Fatalf("ordinary Goal read_file = %+v, %v; want fixture content %q", read, err, readSandbox.output)
	}
	if readSandbox.request.Tool != "read_file" || !strings.HasSuffix(readSandbox.request.Args["file_path"].(string), filepath.Join("project", "fixture.txt")) {
		t.Fatalf("ordinary Goal read_file helper request=%+v", readSandbox.request)
	}
	listed, err := executor.Execute(ctx, llm.ToolUse{
		ID: "restored-goal-team-list", Name: "team_list", Arguments: []byte(`{"limit":10}`),
	})
	if err != nil || listed.Status != agent.ToolSucceeded || listed.IsError || !strings.Contains(listed.Content, team.ID) {
		t.Fatalf("ordinary Goal team_list = %+v, %v; want successful access to team %s", listed, err, team.ID)
	}
	finishTUITestCoordinatorRun(t, ordinaryStream, ordinary)
}

type goalCoordinatorRestoreGate struct{}

func (goalCoordinatorRestoreGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

type goalCoordinatorRestoreSandbox struct {
	output  string
	request execution.HelperRequest
}

func (*goalCoordinatorRestoreSandbox) Probe(context.Context, sandbox.SandboxProfile) error {
	return nil
}

func (s *goalCoordinatorRestoreSandbox) RunIsolated(_ context.Context, _ sandbox.SandboxProfile, _ []string, input io.Reader) (sandbox.SandboxResult, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return sandbox.SandboxResult{}, err
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &s.request); err != nil {
		return sandbox.SandboxResult{}, err
	}
	response, err := json.Marshal(execution.HelperResponse{Output: s.output})
	if err != nil {
		return sandbox.SandboxResult{}, err
	}
	return sandbox.SandboxResult{Stdout: response}, nil
}

func (*goalCoordinatorRestoreSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("unused")
}

func (*goalCoordinatorRestoreSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("unused")
}

func (*goalCoordinatorRestoreSandbox) StopIsolatedSession(context.Context, string) error {
	return errors.New("unused")
}

func openTUITestGoalCoordinatorRun(
	t *testing.T,
	ctx context.Context,
	socket string,
	runner *tuiCoordinatorRecordingRunner,
	work agent.WorkRef,
	intent string,
	coordinatorTeamID string,
) (*conversation.StreamClient, *tuiCoordinatorRunCapture) {
	t.Helper()
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: runID, Work: work, Model: "fixture-model", Intent: intent,
		Messages: []llm.Message{{Role: "user", Content: intent}},
	}
	var stream *conversation.StreamClient
	if coordinatorTeamID != "" {
		stream, err = conversation.OpenGoalCoordinatorRun(ctx, socket, request, coordinatorTeamID)
	} else {
		stream, err = conversation.OpenRun(ctx, socket, request)
	}
	if err != nil {
		t.Fatalf("open Goal run %q: %v", intent, err)
	}
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		_ = stream.Close()
		t.Fatalf("start Goal run %q: message=%+v err=%v", intent, started, err)
	}
	select {
	case capture := <-runner.started:
		if capture.request.RunID != runID || capture.request.Work != work {
			t.Fatalf("runner started scope %+v/run %q, want %+v/run %q", capture.request.Work, capture.request.RunID, work, runID)
		}
		return stream, capture
	case <-ctx.Done():
		_ = stream.Close()
		t.Fatalf("wait for Goal runner %q: %v", intent, ctx.Err())
	case <-time.After(5 * time.Second):
		_ = stream.Close()
		t.Fatalf("runner did not start Goal run %q", intent)
	}
	return nil, nil
}
