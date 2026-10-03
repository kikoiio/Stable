package conversation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type integrationProvider struct{}

func (integrationProvider) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event, 4)
	errors := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errors)
		last := ""
		if len(request.Messages) > 0 {
			last = request.Messages[len(request.Messages)-1].Content
		}
		if last == "cancel me" {
			events <- llm.Event{Kind: llm.TextDelta, Text: "partial"}
			<-ctx.Done()
			return
		}
		if last == "fail me" {
			events <- llm.Event{Kind: llm.TextDelta, Text: "partial"}
			errors <- &llm.ProviderError{Class: llm.ErrorNetwork, Message: "fixture network failure", Retryable: true}
			return
		}
		events <- llm.Event{Kind: llm.ThinkingDelta, Text: "thinking"}
		events <- llm.Event{Kind: llm.TextDelta, Text: "answer"}
		zero := 0
		events <- llm.Event{Kind: llm.Usage, Usage: &llm.UsageInfo{InputTokens: &zero}}
		events <- llm.Event{Kind: llm.StreamEnd}
	}()
	return events, errors
}

func TestConcurrentSessionRunsKeepSessionAndRunOwnership(t *testing.T) {
	root := t.TempDir()
	first, err := sessionlog.Create(root, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionlog.Create(root, "second")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(root, "conversation.sock")
	runner := agent.NewRunner(integrationProvider{}, agent.RunnerOptions{MaxRetries: -1})
	svc, err := Serve(ctx, Deps{Store: db, Runner: runner, ProjectRoot: root, SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	type target struct {
		session sessionlog.SessionInfo
		runID   string
		body    string
	}
	targets := []target{{first, "parallel-1", "one"}, {second, "parallel-2", "two"}}
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, item := range targets {
		wg.Add(1)
		go func(targetInfo target) {
			defer wg.Done()
			request := agent.ExecutionRequest{RunID: targetInfo.runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: targetInfo.session.ID}, Intent: targetInfo.body, Messages: []llm.Message{{Role: "user", Content: targetInfo.body}}, Model: "fixture"}
			stream, openErr := OpenRun(ctx, socket, request)
			if openErr != nil {
				errs <- openErr
				return
			}
			defer stream.Close()
			for {
				message, receiveErr := stream.Receive()
				if receiveErr != nil {
					errs <- receiveErr
					return
				}
				if message.Type == "error" {
					errs <- context.Canceled
					return
				}
				if message.Type == "run_outcome" {
					if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
						errs <- context.Canceled
					}
					return
				}
			}
		}(item)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range targets {
		transcript, err := sessionlog.Replay(root, target.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range transcript.Events {
			if event.Type != sessionlog.EventRunEvent {
				continue
			}
			var run sessionlog.RunEvent
			raw, _ := json.Marshal(event.Data)
			if json.Unmarshal(raw, &run) != nil {
				t.Fatal("invalid run event")
			}
			if run.RunID != target.runID || run.SessionID != target.session.ID {
				t.Fatalf("crossed run/session ownership: %+v want run=%s session=%s", run, target.runID, target.session.ID)
			}
		}
	}
}

func TestUnifiedRunIntegrationPersistsSessionAndGoalStreams(t *testing.T) {
	root := t.TempDir()
	session, err := sessionlog.Create(root, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	goalSession, err := sessionlog.Create(root, "goal")
	if err != nil {
		t.Fatal(err)
	}
	cancelSession, err := sessionlog.Create(root, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	failureSession, err := sessionlog.Create(root, "failure")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.CreateGoal(context.Background(), core.Goal{ID: "goal-1", Objective: "integration goal", AllowedRoot: root, AllowedCapabilities: []string{"kicad.repair_connection"}, SourceSessionID: goalSession.ID}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(root, "conversation.sock")
	runner := agent.NewRunner(integrationProvider{}, agent.RunnerOptions{MaxRetries: -1})
	svc, err := Serve(ctx, Deps{Store: db, Runner: runner, ProjectRoot: root, SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	ordinary := agent.ExecutionRequest{RunID: "ordinary-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "hello", Messages: []llm.Message{{Role: "user", Content: "hello"}}, Model: "fixture"}
	stream, err := OpenRun(ctx, socket, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawThinking, sawUsage, sawOutcome bool
	for !sawOutcome {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if message.Type == "run_event" && message.RunEvent != nil {
			sawText = sawText || message.RunEvent.Kind == string(agent.EventTextDelta)
			sawThinking = sawThinking || message.RunEvent.Kind == string(agent.EventThinkingDelta)
			sawUsage = sawUsage || message.RunEvent.Kind == string(agent.EventUsage)
		}
		if message.Type == "run_outcome" {
			sawOutcome = message.Outcome != nil && message.Outcome.Status == agent.RunCompleted
		}
		if message.Type == "error" {
			t.Fatal(message.Error)
		}
	}
	_ = stream.Close()
	if !sawText || !sawThinking || !sawUsage {
		t.Fatalf("ordinary stream incomplete: text=%v thinking=%v usage=%v", sawText, sawThinking, sawUsage)
	}

	goalRequest := agent.ExecutionRequest{RunID: "goal-run", Work: agent.WorkRef{Kind: agent.WorkGoal, SessionID: goalSession.ID, GoalID: "goal-1", WorkItemID: "item-1"}, Intent: "continue goal", Messages: []llm.Message{{Role: "user", Content: "continue"}}, Model: "fixture"}
	outcome, err := (GoalSocketClient{Socket: socket}).RunGoal(ctx, goalRequest)
	if err != nil || outcome.Status != agent.RunCompleted {
		t.Fatalf("goal outcome=%+v err=%v", outcome, err)
	}

	cancelRequest := agent.ExecutionRequest{RunID: "cancel-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: cancelSession.ID}, Intent: "cancel me", Messages: []llm.Message{{Role: "user", Content: "cancel me"}}, Model: "fixture"}
	cancelStream, err := OpenRun(ctx, socket, cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	gotPartial, gotCancelled := false, false
	for !gotCancelled {
		message, receiveErr := cancelStream.Receive()
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if message.Type == "run_event" && message.RunEvent != nil && message.RunEvent.Kind == string(agent.EventTextDelta) && !gotPartial {
			gotPartial = true
			if err = cancelStream.Cancel(cancelSession.ID, "cancel-run"); err != nil {
				t.Fatal(err)
			}
		}
		if message.Type == "run_outcome" {
			gotCancelled = message.Outcome != nil && message.Outcome.Status == agent.RunCancelled
		}
	}
	_ = cancelStream.Close()
	if !gotPartial {
		t.Fatal("cancelled run did not preserve partial text")
	}
	failureRequest := agent.ExecutionRequest{RunID: "failure-run", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: failureSession.ID}, Intent: "fail me", Messages: []llm.Message{{Role: "user", Content: "fail me"}}, Model: "fixture"}
	failureStream, err := OpenRun(ctx, socket, failureRequest)
	if err != nil {
		t.Fatal(err)
	}
	failurePartial, failureTerminal := false, false
	for !failureTerminal {
		message, receiveErr := failureStream.Receive()
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if message.Type == "run_event" && message.RunEvent != nil {
			if message.RunEvent.Kind == string(agent.EventTextDelta) {
				failurePartial = true
			}
			if message.RunEvent.Kind == string(agent.EventTerminal) {
				failureTerminal = true
			}
		}
	}
	_ = failureStream.Close()
	for _, current := range []struct {
		session sessionlog.SessionInfo
		kind    string
		runID   string
	}{{session, "session", "ordinary-run"}, {goalSession, "goal", "goal-run"}, {cancelSession, "session", "cancel-run"}, {failureSession, "session", "failure-run"}} {
		transcript, replayErr := sessionlog.Replay(root, current.session.ID)
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		var started, terminal, text, partial bool
		for _, event := range transcript.Events {
			if event.Type == sessionlog.EventRunStarted {
				var run sessionlog.RunStarted
				if decodeSessionData(event.Data, &run) == nil && run.RunID == current.runID && run.WorkKind == current.kind {
					started = true
				}
			}
			if event.Type == sessionlog.EventRunEvent {
				var run sessionlog.RunEvent
				if decodeSessionData(event.Data, &run) == nil && run.RunID == current.runID {
					text = text || run.Kind == string(agent.EventTextDelta)
					terminal = terminal || run.Kind == string(agent.EventTerminal)
					var payload struct {
						Text string `json:"text"`
					}
					if decodeSessionData(run.Payload, &payload) == nil && payload.Text == "partial" {
						partial = true
					}
				}
			}
		}
		if !started || !terminal || !text || ((current.runID == "cancel-run" || current.runID == "failure-run") && !partial) {
			t.Fatalf("%s run was not replayable: %+v", current.kind, transcript.Events)
		}
	}
	if !failurePartial {
		t.Fatal("failed provider run dropped partial text")
	}
}
