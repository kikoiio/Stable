package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/decision"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestM03SandboxSecretsTrustedFlow(t *testing.T) {
	root := requiredEnv(t, "M03_PROJECT")
	formal := requiredEnv(t, "M03_CANDIDATE")
	candidateRoot := filepath.Join(filepath.Dir(formal), "trusted-candidate")
	runRoot := requiredEnv(t, "M03_RUN_DIR")
	sentinel := requiredEnv(t, "M03_SENTINEL")
	hostSecret := requiredEnv(t, "M03_HOST_SECRET")
	modelSecret := requiredEnv(t, "M03_SECRET")
	secrets := []string{modelSecret, string(readFixture(t, sentinel)), string(readFixture(t, hostSecret))}
	if err := os.Mkdir(candidateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(candidateRoot) })

	var mockMode atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer "+modelSecret {
			t.Errorf("mock provider received wrong authorization header")
		}
		if mockMode.Load() {
			http.Error(w, "provider rejected key "+modelSecret, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"safe response\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	db, err := store.Open(filepath.Join(filepath.Dir(root), "m03-secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session, err := sessionlog.Create(root, "trusted-flow")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(filepath.Dir(root), "m03-secrets.sock")
	model := appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: mock.URL + "/v1", APIKey: modelSecret}
	streamProvider, err := llm.NewProvider(model)
	if err != nil {
		t.Fatal(err)
	}
	serviceProvider := &decision.HTTPProvider{Config: model, Client: &http.Client{Timeout: 5 * time.Second}}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, Provider: serviceProvider, ChatProvider: serviceProvider, Runner: agent.NewRunner(streamProvider, agent.RunnerOptions{MaxRetries: -1}),
		ProviderCredential: modelSecret, ProviderName: "openai-compatible", Model: "mock", ProjectRoot: root, RunRoot: runRoot, SocketPath: socket,
		PermissionService: &permission.PermissionService{Policy: permission.Policy{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	waitForSocket(t, socket)

	authority := permission.Authority{RunID: "m03-secret-run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot, Mode: permission.ModeDefault}
	operation := permission.Operation{ID: "m03-secret-op", Kind: permission.OpWrite, Name: "write-file", Target: filepath.Join(candidateRoot, "board.kicad_sch"), Parameters: json.RawMessage(`{"value":"safe"}`)}
	client, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	decoder := json.NewDecoder(client)
	if err = json.NewEncoder(client).Encode(conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if msg := receiveServiceMessage(t, client, decoder); msg.Type != "transcript" {
		t.Fatalf("session load transcript=%+v", msg)
	}
	if msg := receiveServiceMessage(t, client, decoder); msg.Type != "done" {
		t.Fatalf("session load completion=%+v", msg)
	}
	decision, err := svc.AuthorizeOperation(ctx, authority, operation)
	if err != nil || decision.Kind != permission.DecisionAsk || decision.ApprovalID == "" {
		t.Fatalf("authorization decision=%+v err=%v", decision, err)
	}
	prompt := receiveServiceMessage(t, client, decoder)
	if prompt.Type != "approval_pending" || prompt.Approval == nil || prompt.Approval.Name != operation.Name || prompt.Approval.Target != operation.Target {
		t.Fatalf("authorization prompt=%+v", prompt)
	}

	request := agent.ExecutionRequest{RunID: "m03-secret-stream", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "hello " + modelSecret, Messages: []llm.Message{{Role: "user", Content: "hello " + modelSecret}}, Model: "mock"}
	if err = json.NewEncoder(client).Encode(conversation.ClientMsg{Op: "run_start", SessionID: session.ID, Run: &request}); err != nil {
		t.Fatal(err)
	}
	var runEvents int
	for {
		message := receiveServiceMessage(t, client, decoder)
		if message.Type == "run_event" && message.RunEvent != nil {
			runEvents++
			assertNoSecretMarkers(t, secrets, string(marshal(t, message.RunEvent)))
		}
		if message.Type == "error" {
			assertNoSecretMarkers(t, secrets, message.Error)
			t.Fatalf("trusted stream returned error: %s", message.Error)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				t.Fatalf("stream outcome=%+v", message.Outcome)
			}
			break
		}
	}
	if runEvents == 0 {
		t.Fatal("trusted run emitted no service events")
	}

	mockMode.Store(true)
	failedRun := agent.ExecutionRequest{RunID: "m03-secret-error", Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "cause provider failure", Messages: []llm.Message{{Role: "user", Content: "cause provider failure"}}, Model: "mock"}
	if err = json.NewEncoder(client).Encode(conversation.ClientMsg{Op: "run_start", SessionID: session.ID, Run: &failedRun}); err != nil {
		t.Fatal(err)
	}
	var failedEvents int
	for {
		message := receiveServiceMessage(t, client, decoder)
		if message.Type == "error" {
			assertNoSecretMarkers(t, secrets, message.Error)
		}
		if message.Type == "run_event" && message.RunEvent != nil {
			failedEvents++
			assertNoSecretMarkers(t, secrets, string(marshal(t, message.RunEvent)))
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunFailed {
				t.Fatalf("failed outcome=%+v", message.Outcome)
			}
			if message.Outcome.Error != nil {
				assertNoSecretMarkers(t, secrets, message.Outcome.Error.Message)
			}
			break
		}
	}
	if failedEvents == 0 {
		t.Fatal("failed provider run emitted no persistent service events")
	}
	assertNoSecretMarkers(t, secrets, string(marshal(t, prompt)), string(marshal(t, decision)))
	transcript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecretMarkers(t, secrets, string(marshal(t, transcript)))
	assertNoSecretMarkersInTree(t, secrets, root, candidateRoot, runRoot, filepath.Join(filepath.Dir(runRoot), ".stable-sessions", "logs"))

	profile := sandbox.SandboxProfile{ProjectRoot: root, CandidateRoot: candidateRoot, RunRoot: runRoot, Timeout: 10 * time.Second, OutputLimit: 4096}
	probe, probeErr := (sandbox.New()).RunIsolated(ctx, profile, []string{"python3", "-c", "import os; print(' '.join(os.environ.values())); print(open('/proc/self/mountinfo').read())"}, nil)
	assertNoSecretMarkers(t, secrets, string(probe.Stdout), string(probe.Stderr), errorString(probeErr))
	if probeErr != nil || probe.ExitCode != 0 {
		t.Fatalf("secret boundary probe failed: exit=%d err=%v", probe.ExitCode, probeErr)
	}
}

func receiveServiceMessage(t *testing.T, conn net.Conn, decoder *json.Decoder) conversation.ServerMsg {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var message conversation.ServerMsg
	if err := decoder.Decode(&message); err != nil {
		t.Fatal(err)
	}
	return message
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("trusted service socket did not appear: %s", path)
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
