package e2e

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sandbox"
	"stable/internal/sessionlog"
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

// m04RealExecutor builds a tool executor backed by the real Linux sandbox.
// It returns the executor plus the formal root, the host sentinel path planted
// outside every authorized root, and the session log root.
func m04RealExecutor(t *testing.T, runID, credential string) (agent.RunExecutor, string, string, string) {
	t.Helper()
	if os.Getenv("STABLE_M04_HELPER") == "" {
		t.Skip("positive tool path requires STABLE_M04_HELPER")
	}
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidate := filepath.Join(root, "candidate")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{formal, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "fixture.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("m04-host-sentinel-"+runID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(sessionRoot, "chat")
	if err != nil {
		t.Fatal(err)
	}
	authority := permission.Authority{RunID: runID, SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidate, Mode: permission.ModeBypass}
	raw, _ := json.Marshal(authority)
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.LinuxManager{}, Gate: m04AllowGate{}, HelperPath: os.Getenv("STABLE_M04_HELPER"), SessionRoot: sessionRoot, Now: time.Now, ProviderCredential: credential})
	runner, err := factory.ForRun(agent.ExecutionRequest{RunID: authority.RunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: authority.SessionID}, Intent: "m04", Model: "test", PermissionBounds: raw})
	if err != nil {
		t.Fatal(err)
	}
	return runner, formal, sentinel, sessionRoot
}

func m04Execute(t *testing.T, runner agent.RunExecutor, id, name, args string) agent.ToolOutcome {
	t.Helper()
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: id, Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return outcome
}

func TestM04SandboxPositive(t *testing.T) {
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
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.LinuxManager{}, Gate: m04AllowGate{}, HelperPath: os.Getenv("STABLE_M04_HELPER"), Now: time.Now})
	if os.Getenv("STABLE_M04_HELPER") == "" {
		t.Skip("positive tool path requires STABLE_M04_HELPER")
	}
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

// C13/C16: path escapes and host sentinels must stay unreadable through every
// tool surface, with readable refusal reasons.
func TestM04SandboxEscapeTrio(t *testing.T) {
	runner, formal, sentinel, _ := m04RealExecutor(t, "m04-escape", "")
	sentinelBytes, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	marker := strings.TrimSpace(string(sentinelBytes))
	if err = os.Symlink(sentinel, filepath.Join(formal, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ id, name, args string }{
		{"dotdot", "read_file", `{"file_path":"../outside/sentinel.txt"}`},
		{"absolute", "read_file", `{"file_path":"` + sentinel + `"}`},
		{"symlink", "read_file", `{"file_path":"link.txt"}`},
		{"write-escape", "write_file", `{"file_path":"../outside/pwned.txt","content":"x"}`},
		{"edit-escape", "edit_file", `{"file_path":"../outside/sentinel.txt","old_string":"m04","new_string":"x"}`},
	}
	for _, tc := range cases {
		outcome := m04Execute(t, runner, tc.id, tc.name, tc.args)
		if !outcome.IsError {
			t.Fatalf("%s unexpectedly succeeded: %#v", tc.id, outcome)
		}
		if outcome.Content == "" {
			t.Fatalf("%s refused without a readable reason", tc.id)
		}
		if strings.Contains(outcome.Content, marker) {
			t.Fatalf("%s leaked sentinel contents: %q", tc.id, outcome.Content)
		}
	}
	// The host sentinel path must not exist inside the command sandbox either.
	cmd := m04Execute(t, runner, "command", "command", `{"command":"cat `+sentinel+` 2>&1 || true; ls /workspace"}`)
	if strings.Contains(cmd.Content, marker) {
		t.Fatalf("command leaked sentinel contents: %q", cmd.Content)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(sentinel), "pwned.txt")); !os.IsNotExist(statErr) {
		t.Fatal("escaping write created a file outside the authorized roots")
	}
}

// C09/C18: command timeout is observable, the sandboxed process tree is gone
// afterwards, and conventional non-zero exits surface as results, not executor
// errors.
func TestM04CommandTimeoutAndExitSemantics(t *testing.T) {
	runner, _, _, _ := m04RealExecutor(t, "m04-timeout", "")
	// Unique sleep durations act as process markers on the host.
	countSleeps := func() int {
		out, _ := exec.Command("pgrep", "-fc", "sleep 3199").Output()
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return n
	}
	before := countSleeps()
	timed := m04Execute(t, runner, "slow", "command", `{"command":"sleep 31991 & sleep 31992 & wait","timeout":2}`)
	if timed.Status != agent.ToolTimeout {
		t.Fatalf("timeout outcome = %#v", timed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countSleeps() != before {
		time.Sleep(100 * time.Millisecond)
	}
	if got := countSleeps(); got != before {
		t.Fatalf("sandbox left %d stray sleep processes (before=%d)", got, before)
	}
	grepMiss := m04Execute(t, runner, "grep-miss", "command", `{"command":"grep -q zzz-no-such-line /workspace/project/fixture.txt"}`)
	if grepMiss.IsError || !strings.Contains(grepMiss.Content, "Exit code 1") {
		t.Fatalf("grep miss should surface exit code 1 as a result: %#v", grepMiss)
	}
	grepHit := m04Execute(t, runner, "grep-hit", "command", `{"command":"grep -c before /workspace/project/fixture.txt"}`)
	if grepHit.IsError || !strings.Contains(grepHit.Content, "Exit code 0") {
		t.Fatalf("grep hit = %#v", grepHit)
	}
}

// C19: the model credential must not reach the sandboxed environment, tool
// outputs that contain it are redacted, and the persisted session log stays
// clean.
func TestM04ToolFlowScrubsSecrets(t *testing.T) {
	credential := "m04-fake-model-key-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Setenv("STABLE_API_KEY", credential)
	runner, _, _, sessionRoot := m04RealExecutor(t, "m04-secrets", credential)
	env := m04Execute(t, runner, "env", "command", `{"command":"env"}`)
	if env.IsError {
		t.Fatalf("env command failed: %#v", env)
	}
	if strings.Contains(env.Content, credential) || strings.Contains(env.Content, "STABLE_API_KEY") {
		t.Fatalf("sandbox environment leaked the model credential: %q", env.Content)
	}
	echo := m04Execute(t, runner, "echo", "command", `{"command":"printf '%s' `+credential+`"}`)
	if strings.Contains(echo.Content, credential) || !strings.Contains(echo.Content, "[credential redacted]") {
		t.Fatalf("tool output was not redacted: %q", echo.Content)
	}
	// The persisted session log must not contain the raw credential either.
	err := filepath.WalkDir(sessionRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), credential) {
			t.Errorf("persisted record %s contains the raw credential", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
