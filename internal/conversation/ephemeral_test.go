package conversation

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestDiscardOnlyIdleEphemeralSession(t *testing.T) {
	root := t.TempDir()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	svc := &Service{deps: Deps{ProjectRoot: t.TempDir(), Store: state}, activeRuns: map[string]string{}, ephemeralRoots: map[string]string{}}
	created, err := svc.handle(context.Background(), ClientMsg{Op: "session_create", ProjectRoot: root, Ephemeral: true})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create = %+v, err=%v", created, err)
	}
	id := created[0].Session.ID
	svc.mu.Lock()
	svc.activeRuns["run"] = id
	svc.mu.Unlock()
	if _, err = svc.discardSession(ClientMsg{ProjectRoot: root, SessionID: id}); err == nil {
		t.Fatal("active ephemeral session was discarded")
	}
	svc.mu.Lock()
	delete(svc.activeRuns, "run")
	svc.mu.Unlock()
	if _, err = svc.discardSession(ClientMsg{ProjectRoot: root, SessionID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Replay(root, id); err == nil {
		t.Fatal("transcript remains after discard")
	}
	ordinary, err := sessionlog.Create(root, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.discardSession(ClientMsg{ProjectRoot: root, SessionID: ordinary.ID}); err == nil {
		t.Fatal("persistent session was discarded")
	}
}

func TestEphemeralPrintRunUsesRequestedProjectAndCleansTranscript(t *testing.T) {
	root, serviceRoot := t.TempDir(), t.TempDir()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "chat.sock")
	runner := agent.NewRunner(integrationProvider{}, agent.RunnerOptions{MaxRetries: -1})
	svc, err := Serve(ctx, Deps{Store: state, Runner: runner, ProjectRoot: serviceRoot, SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	probe, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	created, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: root, Ephemeral: true})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	id := created[0].Session.ID
	if !created[0].Session.Ephemeral {
		t.Fatal("session_create did not preserve the ephemeral marker")
	}
	if got := svc.sessionProjectRoot(id); got != root {
		t.Fatalf("ephemeral session root = %q, want %q", got, root)
	}
	request := agent.ExecutionRequest{RunID: "print-integration", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: id}, Intent: "inspect", Model: "fixture", Messages: []llm.Message{{Role: "user", Content: "print fixture"}}}
	stream, err := OpenRun(ctx, socket, request)
	if err != nil {
		t.Fatal(err)
	}
	var answer string
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			stream.Close()
			t.Fatal(receiveErr)
		}
		if message.Type == "error" {
			stream.Close()
			t.Fatalf("run error: %s", message.Error)
		}
		if message.Type == "run_event" && message.RunEvent != nil && message.RunEvent.Kind == string(agent.EventTextDelta) {
			var delta struct {
				Text string `json:"text"`
			}
			raw, _ := json.Marshal(message.RunEvent.Payload)
			if json.Unmarshal(raw, &delta) == nil {
				answer += delta.Text
			}
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				stream.Close()
				t.Fatalf("outcome=%+v", message.Outcome)
			}
			break
		}
	}
	stream.Close()
	if answer != "answer" {
		t.Fatalf("answer = %q", answer)
	}
	if _, err = sessionlog.Replay(root, id); err != nil {
		t.Fatalf("ephemeral transcript missing before discard: %v", err)
	}
	if _, err = Request(ctx, socket, ClientMsg{Op: "session_discard", ProjectRoot: root, SessionID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err = sessionlog.Replay(root, id); err == nil {
		t.Fatal("transcript remains after discard")
	}
	if sessions, listErr := sessionlog.List(root); listErr != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%+v err=%v", sessions, listErr)
	}
}
