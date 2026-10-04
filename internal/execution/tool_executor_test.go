package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sandbox"
)

type executorTestGate struct {
	decision permission.PermissionDecision
	seen     permission.Operation
}

func (g *executorTestGate) Authorize(_ context.Context, _ permission.Authority, op permission.Operation) (permission.PermissionDecision, error) {
	g.seen = op
	return g.decision, nil
}

type executorTestSandbox struct {
	result  sandbox.SandboxResult
	err     error
	profile sandbox.SandboxProfile
	argv    []string
}

func (s *executorTestSandbox) Probe(context.Context, sandbox.SandboxProfile) error { return nil }
func (s *executorTestSandbox) RunIsolated(_ context.Context, p sandbox.SandboxProfile, argv []string, _ io.Reader) (sandbox.SandboxResult, error) {
	s.profile = p
	s.argv = append([]string(nil), argv...)
	return s.result, s.err
}
func (s *executorTestSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("unused")
}
func (s *executorTestSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("unused")
}
func (s *executorTestSandbox) StopIsolatedSession(context.Context, string) error { return nil }

func executorRequest(t *testing.T, formal, candidateRoot string) agent.ExecutionRequest {
	t.Helper()
	authority := permission.Authority{
		RunID:         "run-1",
		SessionID:     "0123456789abcdef0123456789abcdef",
		AllowedRoot:   formal,
		FormalRoot:    formal,
		CandidateRoot: candidateRoot,
		Mode:          permission.ModeBypass,
	}
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	return agent.ExecutionRequest{RunID: authority.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID}, Intent: "test", Model: "test", PermissionBounds: raw}
}

func TestToolExecutorMapsRelativePathsToFormalAndCandidateRoots(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	request := executorRequest(t, formal, candidateRoot)
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	executor := runner.(*toolRunExecutor)

	args := map[string]any{"file_path": "src/main.go"}
	_, kind, _, rel, err := executor.mapTool("read_file", args)
	if err != nil || kind != permission.OpRead || rel != filepath.Clean("src/main.go") {
		t.Fatalf("read mapping = kind %q rel %q err %v", kind, rel, err)
	}
	if got, want := args["file_path"], filepath.Join("/workspace/project", "src/main.go"); got != want {
		t.Fatalf("read guest path = %v, want %v", got, want)
	}

	args = map[string]any{"file_path": "src/main.go", "content": "updated"}
	_, kind, _, rel, err = executor.mapTool("write_file", args)
	if err != nil || kind != permission.OpWrite || rel != filepath.Clean("src/main.go") {
		t.Fatalf("write mapping = kind %q rel %q err %v", kind, rel, err)
	}
	if got, want := args["file_path"], filepath.Join("/workspace/candidate", "src/main.go"); got != want {
		t.Fatalf("write guest path = %v, want %v", got, want)
	}
}

func TestToolExecutorRejectsUnsafeRelativePath(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	request := executorRequest(t, formal, candidateRoot)
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"../secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolFailed || !outcome.IsError {
		t.Fatalf("unsafe path outcome = %#v", outcome)
	}
}

func TestToolExecutorUnknownToolIsToolFailure(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	factory := NewToolExecutorFactory(ToolExecutorDeps{Now: time.Now})
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "call-unknown", Name: "no_such_tool", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolFailed || !outcome.IsError || outcome.Content != `Error: unknown tool "no_such_tool"` {
		t.Fatalf("unknown tool outcome = %#v", outcome)
	}
}

func TestToolExecutorSandboxUnavailableIsDenied(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{err: sandbox.ErrUnavailable}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, Now: time.Now})
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "command", Arguments: json.RawMessage(`{"command":"pwd"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !outcome.IsError {
		t.Fatalf("unavailable outcome = %#v", outcome)
	}
	if gate.seen.Kind != permission.OpCommand || gate.seen.Name != "Command" {
		t.Fatalf("permission operation = %#v", gate.seen)
	}
}

func TestToolExecutorEnforcesReadBeforeWrite(t *testing.T) {
	formal := t.TempDir()
	if err := os.WriteFile(filepath.Join(formal, "a.txt"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "cand")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte(`{"output":"done"}`), ExitCode: 0}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, HelperPath: "helper", Now: time.Now})
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	// The candidate copy exists but was never read in this run: refuse.
	blind, err := runner.Execute(context.Background(), llm.ToolUse{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"a.txt","content":"new\n"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !blind.IsError || !strings.Contains(blind.Content, "has not been read") {
		t.Fatalf("blind overwrite outcome = %#v", blind)
	}
	if sandboxFake.argv != nil {
		t.Fatalf("blind overwrite reached the sandbox: %v", sandboxFake.argv)
	}
	// After a successful read the same write is dispatched.
	read, err := runner.Execute(context.Background(), llm.ToolUse{ID: "r1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"a.txt"}`)})
	if err != nil || read.IsError {
		t.Fatalf("read = %#v, err=%v", read, err)
	}
	write, err := runner.Execute(context.Background(), llm.ToolUse{ID: "w2", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"a.txt","content":"new\n"}`)})
	if err != nil || write.IsError {
		t.Fatalf("write after read = %#v, err=%v", write, err)
	}
	if sandboxFake.argv == nil {
		t.Fatal("write after read was not dispatched to the sandbox")
	}
	// Writing a brand-new path does not require a prior read.
	fresh, err := runner.Execute(context.Background(), llm.ToolUse{ID: "w3", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"new.txt","content":"x\n"}`)})
	if err != nil || fresh.IsError {
		t.Fatalf("fresh write = %#v, err=%v", fresh, err)
	}
}
