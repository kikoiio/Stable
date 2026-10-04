package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sandbox"
)

type m04AllowGate struct{}

func (m04AllowGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow, Reason: "m04 test grant"}, nil
}

type m04Probe struct{ err error }

func (m m04Probe) Probe(context.Context, sandbox.SandboxProfile) error { return m.err }
func (m m04Probe) RunIsolated(context.Context, sandbox.SandboxProfile, []string, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, m.err
}
func (m m04Probe) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, m.err
}
func (m m04Probe) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, m.err
}
func (m04Probe) StopIsolatedSession(context.Context, string) error { return nil }

func TestM04ToolExecutorFailClosed(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidate := filepath.Join(root, "candidate")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{RunID: "m04-deny", SessionID: "m04-session", AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidate, Mode: permission.ModeBypass}
	raw, _ := json.Marshal(authority)
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: m04Probe{err: sandbox.ErrUnavailable}, Gate: m04AllowGate{}, Now: time.Now})
	runner, err := factory.ForRun(agent.ExecutionRequest{RunID: authority.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID}, Intent: "m04", Model: "test", PermissionBounds: raw})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "m04-call", Name: "command", Arguments: json.RawMessage(`{"command":"pwd"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !strings.Contains(outcome.Content, "isolation unavailable") {
		t.Fatalf("fail-closed outcome = %#v", outcome)
	}
}

func TestM04SandboxPositive(t *testing.T) {
	if os.Getenv("STABLE_M04_HELPER") == "" {
		t.Skip("positive tool path requires STABLE_M04_HELPER")
	}
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidate := filepath.Join(root, "candidate")
	runRoot := filepath.Join(root, "run")
	for _, dir := range []string{formal, runRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "fixture.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{RunID: "m04-positive", SessionID: "m04-session", AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidate, Mode: permission.ModeBypass}
	raw, _ := json.Marshal(authority)
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.LinuxManager{}, Gate: m04AllowGate{}, HelperPath: os.Getenv("STABLE_M04_HELPER"), SessionRoot: filepath.Join(root, "sessions"), Now: time.Now})
	runner, err := factory.ForRun(agent.ExecutionRequest{RunID: authority.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID}, Intent: "m04", Model: "test", PermissionBounds: raw})
	if err != nil {
		t.Fatal(err)
	}
	read, err := runner.Execute(context.Background(), llm.ToolUse{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"fixture.txt"}`)})
	if err != nil || read.IsError || !strings.Contains(read.Content, "before") {
		t.Fatalf("read = %#v, err=%v", read, err)
	}
	write, err := runner.Execute(context.Background(), llm.ToolUse{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"fixture.txt","content":"after\n"}`)})
	if err != nil || write.IsError || write.Diff == nil || write.Diff.Additions == 0 || write.Diff.Removals == 0 {
		t.Fatalf("write = %#v, err=%v", write, err)
	}
	command, err := runner.Execute(context.Background(), llm.ToolUse{ID: "command", Name: "command", Arguments: json.RawMessage(`{"command":"test -f /workspace/candidate/fixture.txt && test ! -f /workspace/project/fixture.txt && printf ok"}`)})
	if err != nil || command.IsError || !strings.Contains(command.Content, "ok") {
		t.Fatalf("command = %#v, err=%v", command, err)
	}
}
