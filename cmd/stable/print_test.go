package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/appconfig"
	"stable/internal/conversation"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type printFailReader struct{}

func (printFailReader) Read([]byte) (int, error) { return 0, errors.New("stdin failed") }

func TestReadPrintInput(t *testing.T) {
	input, err := readPrintInput([]string{"summarize", "this"}, strings.NewReader("ignored"))
	if err != nil || input != "summarize this" {
		t.Fatalf("input=%q err=%v", input, err)
	}
	input, err = readPrintInput(nil, strings.NewReader("from stdin\n"))
	if err != nil || input != "from stdin\n" {
		t.Fatalf("stdin input=%q err=%v", input, err)
	}
	if _, err = readPrintInput(nil, printFailReader{}); err == nil {
		t.Fatal("stdin read error was ignored")
	}
	if _, err = readPrintInput(nil, strings.NewReader(" \n")); err == nil {
		t.Fatal("empty input accepted")
	}
}

func TestSafePrintTextRedactsCredential(t *testing.T) {
	if got := safePrintText("answer sk-secret remains", "sk-secret"); got != "answer [REDACTED] remains" {
		t.Fatalf("redacted text = %q", got)
	}
	if got := safePrintText("answer", ""); got != "answer" {
		t.Fatalf("empty credential changed text: %q", got)
	}
}

func TestGeminiPrintRejectedBeforeRuntime(t *testing.T) {
	if err := validatePrintProvider("gemini"); err == nil || !strings.Contains(err.Error(), "target decisions only") {
		t.Fatalf("Gemini validation error = %v", err)
	}
	for _, provider := range []string{"openai", "anthropic", "openai-compatible"} {
		if err := validatePrintProvider(provider); err != nil {
			t.Errorf("%s rejected: %v", provider, err)
		}
	}
}

type printCLIFakeProvider struct {
	answer string
	seen   chan string
	block  bool
	tool   string
	fail   bool
}

func (p printCLIFakeProvider) Stream(ctx context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event, 3)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		if p.seen != nil && len(request.Messages) > 0 {
			p.seen <- request.Messages[len(request.Messages)-1].Content
		}
		if p.tool != "" {
			args := []byte(`{"file_path":"print-denied.txt","content":"must not be written"}`)
			events <- llm.Event{Kind: llm.ToolCallComplete, Tool: &llm.ToolCall{ID: "print-tool", Name: p.tool, Arguments: args, Complete: true}}
			events <- llm.Event{Kind: llm.StreamEnd}
			return
		}
		if p.answer != "" {
			events <- llm.Event{Kind: llm.TextDelta, Text: p.answer}
		}
		if p.block {
			<-ctx.Done()
			return
		}
		if p.fail {
			errs <- &llm.ProviderError{Class: llm.ErrorNetwork, Message: "fixture failure"}
			return
		}
		events <- llm.Event{Kind: llm.StreamEnd}
	}()
	return events, errs
}

func TestPrintCLIWithConversationServiceStartsAndDiscardsEphemeralSession(t *testing.T) {
	const secret = "sk-cli-print-secret"
	seen := make(chan string, 1)
	root := startPrintCLIHarness(t, printCLIFakeProvider{answer: "answer " + secret, seen: seen}, false)
	var stdout, stderr bytes.Buffer
	if err := runPrint(nil, strings.NewReader("inspect this workspace"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "inspect this workspace" {
		t.Fatalf("provider received stdin prompt %q", got)
	}
	if printCLIRuntimeUpCalls != 1 {
		t.Fatalf("runtime startup calls = %d", printCLIRuntimeUpCalls)
	}
	if stdout.String() != "answer [REDACTED]" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	assertPrintCLICleanup(t, root)
}

func TestPrintCLICleansSessionAfterProviderFailure(t *testing.T) {
	root := startPrintCLIHarness(t, printCLIFakeProvider{answer: "partial", fail: true}, false)
	var stdout, stderr bytes.Buffer
	err := runPrint([]string{"fail now"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "fixture failure") {
		t.Fatalf("print failure = %v", err)
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "sk-cli-print-secret") {
		t.Fatalf("stderr leaked credential: %q", stderr.String())
	}
	assertPrintCLICleanup(t, root)
}

func TestPrintCLICancelsAndCleansAfterInterrupt(t *testing.T) {
	seen := make(chan string, 1)
	root := startPrintCLIHarness(t, printCLIFakeProvider{seen: seen, block: true}, false)
	interrupt := make(chan os.Signal, 1)
	var stdout, stderr bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- runPrintWithInterrupts([]string{"wait"}, strings.NewReader(""), &stdout, &stderr, interrupt)
	}()
	select {
	case <-seen:
		interrupt <- os.Interrupt
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("interrupted print error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("print did not finish after interrupt")
	}
	assertPrintCLICleanup(t, root)
}

func TestPrintCLIRejectsWriteToolAndCleans(t *testing.T) {
	root := startPrintCLIHarness(t, printCLIFakeProvider{tool: "write_file"}, true)
	var stdout, stderr bytes.Buffer
	err := runPrint([]string{"write a file"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not allowed in print") {
		t.Fatalf("write tool print error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected answer text: %q", stdout.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, "print-denied.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("write tool produced a file: %v", statErr)
	}
	assertPrintCLICleanup(t, root)
}

func TestPrintCLIRejectsAskUserWithoutWaitingAndCleans(t *testing.T) {
	root := startPrintCLIHarness(t, printCLIFakeProvider{tool: "ask_user"}, true)
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runPrint([]string{"ask a question"}, strings.NewReader(""), &stdout, &stderr)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not allowed in print") {
			t.Fatalf("ask_user print error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("print waited for an ask_user response")
	}
	assertPrintCLICleanup(t, root)
}

var printCLIRuntimeUpCalls int

func startPrintCLIHarness(t *testing.T, provider llm.Provider, withExecutor bool) string {
	t.Helper()
	root, stateDir, serviceRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })
	configDir := t.TempDir()
	if err = os.Chmod(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	configJSON := `{"state_dir":"` + filepath.ToSlash(stateDir) + `","model":{"provider":"openai","model":"fake","api_key":"sk-cli-print-secret"}}`
	if err = os.WriteFile(configPath, []byte(configJSON), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STABLE_CONFIG", configPath)
	for _, key := range []string{"STABLE_PROVIDER", "STABLE_MODEL", "STABLE_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(key, "")
	}
	p, err := paths.Resolve(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(p.Database)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var factory agent.ExecutorFactory
	if withExecutor {
		factory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{ProviderCredential: "sk-cli-print-secret"})
	}
	runner := agent.NewRunner(provider, agent.RunnerOptions{MaxRetries: -1, ExecutorFactory: factory})
	svc, err := conversation.Serve(ctx, conversation.Deps{Store: db, Runner: runner, ProjectRoot: serviceRoot, SocketPath: p.ChatSocket})
	if err != nil {
		cancel()
		_ = db.Close()
		t.Fatal(err)
	}
	probe, err := net.DialTimeout("unix", p.ChatSocket, time.Second)
	if err != nil {
		_ = svc.Close()
		cancel()
		_ = db.Close()
		t.Fatal(err)
	}
	_ = probe.Close()
	t.Cleanup(func() { _ = svc.Close(); cancel(); _ = db.Close() })
	oldControl, oldUp, oldCalls := printRuntimeControl, printRuntimeUp, printCLIRuntimeUpCalls
	t.Cleanup(func() { printRuntimeControl, printRuntimeUp, printCLIRuntimeUpCalls = oldControl, oldUp, oldCalls })
	printCLIRuntimeUpCalls = 0
	printRuntimeControl = func(_ paths.Paths, _ string) (runtime.Status, error) {
		return runtime.Status{}, errors.New("runtime is not running")
	}
	printRuntimeUp = func(_ context.Context, _ appconfig.AppConfig, _ paths.Paths) (runtime.Status, error) {
		printCLIRuntimeUpCalls++
		return runtime.Status{Running: true}, nil
	}
	return root
}

func assertPrintCLICleanup(t *testing.T, root string) {
	t.Helper()
	sessions, err := sessionlog.List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("print left persistent session entries: %+v", sessions)
	}
	transcriptPath, err := sessionlog.SessionPath(root, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(transcriptPath))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("print left session transcripts: %+v", entries)
	}
}
