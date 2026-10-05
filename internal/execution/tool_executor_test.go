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
	"stable/internal/sessionlog"
)

type executorTestGate struct {
	decision permission.PermissionDecision
	seen     permission.Operation
	calls    int
}

func (g *executorTestGate) Authorize(_ context.Context, _ permission.Authority, op permission.Operation) (permission.PermissionDecision, error) {
	g.calls++
	g.seen = op
	return g.decision, nil
}

type executorTestHookRunner struct {
	preRejected bool
	preHookID   string
	preMessage  string
	preCalls    int
	postCalls   int
	preArgs     map[string]any
	postArgs    map[string]any
	postContent string
	sequence    *[]string
}

func (h *executorTestHookRunner) PreToolUse(_ string, _ string, args map[string]any) (bool, string, string) {
	h.preCalls++
	h.preArgs = args
	if h.sequence != nil {
		*h.sequence = append(*h.sequence, "pre")
	}
	return h.preRejected, h.preHookID, h.preMessage
}

func (h *executorTestHookRunner) PostToolUse(_ string, _ string, args map[string]any, result string) {
	h.postCalls++
	h.postArgs = args
	h.postContent = result
	if h.sequence != nil {
		*h.sequence = append(*h.sequence, "post")
	}
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

func TestToolExecutorHookRunner(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	hooks := &executorTestHookRunner{preRejected: true, preHookID: "deny-command", preMessage: "commands are disabled"}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Now: time.Now}, WithHookRunner(hooks))
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "command", Arguments: json.RawMessage(`{"command":"pwd"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !outcome.IsError || outcome.Content != "Blocked by hook deny-command: commands are disabled" {
		t.Fatalf("hook rejection outcome = %#v", outcome)
	}
	if gate.calls != 0 {
		t.Fatalf("permission gate called %d times after hook rejection", gate.calls)
	}
	if hooks.preCalls != 1 || hooks.postCalls != 0 {
		t.Fatalf("hook calls = pre %d post %d, want 1/0", hooks.preCalls, hooks.postCalls)
	}
}

func TestToolExecutorHookRunnerPostsFinalizedContent(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	credential := "secret-token"
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte(""), Stderr: []byte(strings.Repeat("x", toolOutputLimit+1) + credential), ExitCode: 0}}
	hooks := &executorTestHookRunner{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, Now: time.Now, ProviderCredential: credential}, WithHookRunner(hooks))
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "command", Arguments: json.RawMessage(`{"command":"pwd"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if hooks.preCalls != 1 || hooks.postCalls != 1 {
		t.Fatalf("hook calls = pre %d post %d, want 1/1", hooks.preCalls, hooks.postCalls)
	}
	if hooks.postContent != outcome.Content || len(hooks.postContent) > toolOutputLimit+len("\n[output truncated]") || strings.Contains(hooks.postContent, credential) {
		t.Fatalf("post content was not finalized: %q", hooks.postContent)
	}
}

func TestToolExecutorHookRunnerSkipsPostOnCancellation(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{err: context.Canceled}
	hooks := &executorTestHookRunner{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, Now: time.Now}, WithHookRunner(hooks))
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = runner.Execute(ctx, llm.ToolUse{ID: "call-1", Name: "command", Arguments: json.RawMessage(`{"command":"pwd"}`)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled command err = %v", err)
	}
	if hooks.preCalls != 1 || hooks.postCalls != 0 {
		t.Fatalf("hook calls = pre %d post %d, want 1/0", hooks.preCalls, hooks.postCalls)
	}
}

func TestToolExecutorHookRunnerRunsBeforePermission(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	sequence := []string{}
	gate := &sequenceGate{decisions: []permission.PermissionDecision{{Kind: permission.DecisionDeny}}, sequence: &sequence}
	hooks := &executorTestHookRunner{sequence: &sequence}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Now: time.Now}, WithHookRunner(hooks))
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Execute(context.Background(), llm.ToolUse{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"a.txt"}`)}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(sequence, ","), "pre,gate,post"; got != want {
		t.Fatalf("call sequence = %q, want %q", got, want)
	}
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

type pendingApprovals struct{}

func (pendingApprovals) CreateApproval(context.Context, permission.ApprovalRequest) error { return nil }
func (pendingApprovals) GetApproval(context.Context, string) (permission.ApprovalRequest, error) {
	return permission.ApprovalRequest{}, errors.New("unused")
}
func (pendingApprovals) GetApprovalForOperation(context.Context, string, string, string) (permission.ApprovalRequest, bool, error) {
	return permission.ApprovalRequest{Status: permission.ApprovalPending}, true, nil
}
func (pendingApprovals) ResolveApproval(context.Context, string, permission.ApprovalStatus, string, string, *permission.ExactRule) error {
	return errors.New("unused")
}
func (pendingApprovals) CancelApproval(context.Context, string, string) error {
	return errors.New("unused")
}
func (pendingApprovals) ConsumeApproval(context.Context, string, string, string) error {
	return errors.New("unused")
}
func (pendingApprovals) RecordPermissionDecision(context.Context, permission.PermissionDecision, permission.Authority, permission.Operation) error {
	return nil
}

type sequenceGate struct {
	decisions []permission.PermissionDecision
	calls     int
	seen      permission.Operation
	sequence  *[]string
}

func (g *sequenceGate) Authorize(_ context.Context, _ permission.Authority, op permission.Operation) (permission.PermissionDecision, error) {
	if g.sequence != nil {
		*g.sequence = append(*g.sequence, "gate")
	}
	g.seen = op
	i := g.calls
	if i >= len(g.decisions) {
		i = len(g.decisions) - 1
	}
	g.calls++
	return g.decisions[i], nil
}

func TestToolExecutorCancelDuringApprovalClosesSessionlogPair(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "cand")
	sessionRoot := t.TempDir()
	info, err := sessionlog.Create(sessionRoot, "cancel-pairing")
	if err != nil {
		t.Fatal(err)
	}
	gate := &sequenceGate{decisions: []permission.PermissionDecision{
		{Kind: permission.DecisionAsk},
		{Kind: permission.DecisionAllow},
	}}
	sandboxFake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte(`{"output":"done"}`), ExitCode: 0}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, Approvals: pendingApprovals{}, HelperPath: "helper", SessionRoot: sessionRoot, Now: time.Now, PollEvery: time.Millisecond})
	request := executorRequest(t, formal, candidateRoot)
	authority := permission.Authority{RunID: "run-1", SessionID: info.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot, Mode: permission.ModeBypass}
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	request.Work.SessionID = info.ID
	request.PermissionBounds = raw
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call := llm.ToolUse{ID: "call-cancel", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"a.txt","content":"x\n"}`)}
	if _, err = runner.Execute(ctx, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write err = %v", err)
	}
	// The aborted call must be paired in the session log: a later run reusing
	// the same call id must not fail with "duplicate pending tool call id".
	// Real runs get a fresh candidate root per run, so mirror that here.
	request.RunID = "run-2"
	authority.RunID = "run-2"
	authority.CandidateRoot = filepath.Join(t.TempDir(), "cand2")
	raw, err = json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = raw
	runner2, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner2.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("reused call id after cancellation: %v", err)
	}
	if outcome.IsError {
		t.Fatalf("reused call id outcome = %#v", outcome)
	}
	replay, err := sessionlog.Replay(sessionRoot, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls, results := 0, 0
	for _, event := range replay.Events {
		raw, _ := json.Marshal(event.Data)
		switch event.Type {
		case sessionlog.EventToolCall:
			var c sessionlog.ToolCall
			if json.Unmarshal(raw, &c) == nil && c.CallID == "call-cancel" {
				calls++
			}
		case sessionlog.EventToolResult:
			var r sessionlog.ToolResult
			if json.Unmarshal(raw, &r) == nil && r.CallID == "call-cancel" {
				results++
			}
		}
	}
	if calls != 2 || results != 2 {
		t.Fatalf("sessionlog pairing: calls=%d results=%d, want 2/2", calls, results)
	}
}

func TestToolExecutorCreatesCandidateBeforePermissionCheck(t *testing.T) {
	formal := t.TempDir()
	if err := os.WriteFile(filepath.Join(formal, "a.txt"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "cand")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "denied by test"}}
	sandboxFake := &executorTestSandbox{}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, HelperPath: "helper", Now: time.Now})
	runner, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"a.txt","content":"new\n"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !outcome.IsError {
		t.Fatalf("denied write outcome = %#v", outcome)
	}
	// The gate resolves write targets against the candidate root, so the
	// workspace must already exist when Authorize runs — otherwise the policy
	// denies with "target path cannot be safely resolved" before any ask.
	if _, statErr := os.Stat(candidateRoot); statErr != nil {
		t.Fatalf("candidate root missing when the gate evaluated the write: %v", statErr)
	}
	if want := filepath.Join(candidateRoot, "a.txt"); gate.seen.Target != want {
		t.Fatalf("gate target = %q, want %q", gate.seen.Target, want)
	}
	if sandboxFake.argv != nil {
		t.Fatalf("denied write reached the sandbox: %v", sandboxFake.argv)
	}
}
