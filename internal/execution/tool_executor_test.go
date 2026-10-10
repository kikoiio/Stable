package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/redact"
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

func (h *executorTestHookRunner) PreToolUseRun(_ context.Context, _ agent.ParentRun, _ string, _ string, args map[string]any) (bool, string, string) {
	h.preCalls++
	h.preArgs = args
	if h.sequence != nil {
		*h.sequence = append(*h.sequence, "pre")
	}
	return h.preRejected, h.preHookID, h.preMessage
}

func (h *executorTestHookRunner) PostToolUseRun(_ context.Context, _ agent.ParentRun, _ string, _ string, args map[string]any, result string) {
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

func TestToolExecutorCommandUsesPinnedOneShotNetworkProfile(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	request := executorRequest(t, formal, candidateRoot)
	var authority permission.Authority
	if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	authority.Network = []permission.NetworkGrant{{Protocol: "tcp", Host: "localhost", Port: 443}}
	request.PermissionBounds, _ = json.Marshal(authority)

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte("ok\n"), ExitCode: 0}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, HelperPath: "proxy-helper", Now: time.Now})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "network-command", Name: "command", Arguments: json.RawMessage(`{"command":"printf ok"}`)})
	if err != nil || outcome.IsError {
		t.Fatalf("network command = %#v, err=%v", outcome, err)
	}
	if len(sandboxFake.profile.NetworkGrants) != 1 {
		t.Fatalf("network grants = %+v", sandboxFake.profile.NetworkGrants)
	}
	grant := sandboxFake.profile.NetworkGrants[0]
	if grant.Protocol != "tcp" || grant.Host != "localhost" || len(grant.ResolvedIPs) == 0 {
		t.Fatalf("grant was not pinned in command profile: %+v", grant)
	}
	wantHelper, err := filepath.Abs("proxy-helper")
	if err != nil {
		t.Fatal(err)
	}
	if sandboxFake.profile.ProxyHelperPath != wantHelper {
		t.Fatalf("proxy helper = %q, want %q", sandboxFake.profile.ProxyHelperPath, wantHelper)
	}
}

func TestToolExecutorHelperUsesPinnedProfileAndCleansScratch(t *testing.T) {
	formal := t.TempDir()
	if err := os.WriteFile(filepath.Join(formal, "a.txt"), []byte("content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	request := executorRequest(t, formal, candidateRoot)
	var authority permission.Authority
	if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil {
		t.Fatal(err)
	}
	authority.Network = []permission.NetworkGrant{{Protocol: "tcp", Host: "localhost", Port: 443}}
	request.PermissionBounds, _ = json.Marshal(authority)

	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	sandboxFake := &executorTestSandbox{result: sandbox.SandboxResult{Stdout: []byte(`{"output":"read"}`), ExitCode: 0}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, Sandbox: sandboxFake, HelperPath: "tool-helper", Now: time.Now})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.Execute(context.Background(), llm.ToolUse{ID: "network-helper", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"a.txt"}`)})
	if err != nil || outcome.IsError {
		t.Fatalf("network helper = %#v, err=%v", outcome, err)
	}
	if len(sandboxFake.profile.NetworkGrants) != 1 || len(sandboxFake.profile.NetworkGrants[0].ResolvedIPs) == 0 {
		t.Fatalf("grant was not pinned in helper profile: %+v", sandboxFake.profile.NetworkGrants)
	}
	if sandboxFake.profile.CandidateRoot == authority.CandidateRoot {
		t.Fatalf("read-only helper did not use scratch candidate: %q", sandboxFake.profile.CandidateRoot)
	}
	if _, statErr := os.Stat(sandboxFake.profile.CandidateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("scratch candidate was not cleaned up: path=%q err=%v", sandboxFake.profile.CandidateRoot, statErr)
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

// ---------------------------------------------------------------------------
// M07-C MCP host entries
// ---------------------------------------------------------------------------

// fakeMCPCaller records the execution-side MCP surface for dispatch tests.
type fakeMCPCaller struct {
	// resolve maps query → normalized (server, tool); a miss produces the
	// same guidance error the real resolver documents.
	resolve map[string][2]string
	// schemas maps "server__tool" → input schema; a miss skips coercion.
	schemas  map[string]map[string]any
	dispatch []MCPToolSchema

	output     string
	isError    bool
	callErr    error
	resolveErr error

	calls []struct {
		server string
		tool   string
		args   map[string]any
	}
	queries  []string
	sequence *[]string
}

func fakeDispatchTools() []MCPToolSchema {
	return []MCPToolSchema{
		{Name: "mcp__github__create_issue", Description: "Create an issue on a repository", InputSchema: map[string]any{"type": "object"}},
		{Name: "mcp__github__list_issues", Description: "List repository issues", InputSchema: map[string]any{"type": "object"}},
		{Name: "mcp__docs__search", Description: "Search documents", InputSchema: map[string]any{"type": "object"}},
	}
}

func newFakeMCPCaller() *fakeMCPCaller {
	return &fakeMCPCaller{
		resolve: map[string][2]string{
			"mcp__github__create_issue": {"github", "create_issue"},
			"github__create_issue":      {"github", "create_issue"},
		},
		schemas:  map[string]map[string]any{},
		dispatch: fakeDispatchTools(),
	}
}

func (c *fakeMCPCaller) CallTool(_ context.Context, server, tool string, args map[string]any) (string, bool, error) {
	if c.sequence != nil {
		*c.sequence = append(*c.sequence, "call")
	}
	c.calls = append(c.calls, struct {
		server string
		tool   string
		args   map[string]any
	}{server, tool, args})
	return c.output, c.isError, c.callErr
}

func (c *fakeMCPCaller) EagerSchemas() []MCPToolSchema { return nil }

func (c *fakeMCPCaller) DispatchTools() []MCPToolSchema { return c.dispatch }

func (c *fakeMCPCaller) InputSchema(server, tool string) (map[string]any, bool) {
	schema, ok := c.schemas[server+"__"+tool]
	return schema, ok
}

func (c *fakeMCPCaller) ResolveTarget(query string) (string, string, error) {
	c.queries = append(c.queries, query)
	if c.resolveErr != nil {
		return "", "", c.resolveErr
	}
	if target, ok := c.resolve[query]; ok {
		return target[0], target[1], nil
	}
	names := make([]string, 0, len(c.dispatch))
	for _, tool := range c.dispatch {
		names = append(names, tool.Name)
	}
	return "", "", fmt.Errorf("unknown MCP tool %q (available: %s)", query, strings.Join(names, ", "))
}

func (c *fakeMCPCaller) Instructions() string { return "" }

// resolvedApprovals reports a non-pending approval so waitForApproval proceeds
// to its re-authorization instead of polling forever.
type resolvedApprovals struct{ status permission.ApprovalStatus }

func (resolvedApprovals) CreateApproval(context.Context, permission.ApprovalRequest) error {
	return nil
}
func (resolvedApprovals) GetApproval(context.Context, string) (permission.ApprovalRequest, error) {
	return permission.ApprovalRequest{}, errors.New("unused")
}
func (a resolvedApprovals) GetApprovalForOperation(context.Context, string, string, string) (permission.ApprovalRequest, bool, error) {
	return permission.ApprovalRequest{Status: a.status}, true, nil
}
func (resolvedApprovals) ResolveApproval(context.Context, string, permission.ApprovalStatus, string, string, *permission.ExactRule) error {
	return nil
}
func (resolvedApprovals) CancelApproval(context.Context, string, string) error          { return nil }
func (resolvedApprovals) ConsumeApproval(context.Context, string, string, string) error { return nil }
func (resolvedApprovals) RecordPermissionDecision(context.Context, permission.PermissionDecision, permission.Authority, permission.Operation) error {
	return nil
}

func TestMCPDirectCallRunsHookGateThenCaller(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	sequence := []string{}
	gate := &sequenceGate{decisions: []permission.PermissionDecision{{Kind: permission.DecisionAllow}}, sequence: &sequence}
	hooks := &executorTestHookRunner{sequence: &sequence}
	caller := newFakeMCPCaller()
	caller.output = "issue created"
	caller.sequence = &sequence
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, HookRunner: hooks, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{"title":"bug"}`))
	requireOutcome(t, outcome, err, false, "issue created")
	if got, want := strings.Join(sequence, ","), "pre,gate,call,post"; got != want {
		t.Fatalf("call sequence = %q, want %q", got, want)
	}
	if gate.seen.Kind != permission.OpMCPTool || gate.seen.Name != "github" || gate.seen.Target != "github__create_issue" {
		t.Fatalf("gate operation = %#v", gate.seen)
	}
	if len(caller.calls) != 1 || caller.calls[0].server != "github" || caller.calls[0].tool != "create_issue" {
		t.Fatalf("caller calls = %#v", caller.calls)
	}
	if got := caller.calls[0].args["title"]; got != "bug" {
		t.Fatalf("call args = %#v", caller.calls[0].args)
	}
}

func TestMCPDirectCallDefaultAskThenDeny(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeBypass, "")
	gate := &sequenceGate{decisions: []permission.PermissionDecision{
		{Kind: permission.DecisionAsk, Reason: "mcp tool operation requires user approval"},
		{Kind: permission.DecisionDeny, Reason: "user denied operation"},
	}}
	caller := newFakeMCPCaller()
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, Approvals: resolvedApprovals{status: permission.ApprovalAllowedOnce}, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{"title":"bug"}`))
	requireOutcome(t, outcome, err, true, "user denied operation")
	if outcome.Status != agent.ToolDenied {
		t.Fatalf("outcome status = %q, want denied", outcome.Status)
	}
	if gate.calls != 2 {
		t.Fatalf("gate calls = %d, want 2 (ask, then re-check after the approval resolved)", gate.calls)
	}
	if len(caller.calls) != 0 {
		t.Fatal("caller must not run for a denied call")
	}
}

func TestMCPDirectCallAskApprovedByApprovalFlow(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeBypass, "")
	gate := &sequenceGate{decisions: []permission.PermissionDecision{
		{Kind: permission.DecisionAsk},
		{Kind: permission.DecisionAllow, Reason: "approved once by user"},
	}}
	caller := newFakeMCPCaller()
	caller.output = "approved and called"
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, Approvals: resolvedApprovals{status: permission.ApprovalAllowedOnce}, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{"title":"bug"}`))
	requireOutcome(t, outcome, err, false, "approved and called")
	if gate.calls != 2 || len(caller.calls) != 1 {
		t.Fatalf("gate calls = %d, caller calls = %d, want 2/1", gate.calls, len(caller.calls))
	}
}

func TestMCPCallDefaultsToAskUnderRealPolicy(t *testing.T) {
	formal := t.TempDir()
	// Bypass mode never auto-allows an MCP tool call, so with no rules the
	// real policy answers ask and the executor lands in the approval wait.
	authority := m06Authority(t, formal, permission.ModeBypass, "")
	caller := newFakeMCPCaller()
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{}, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{"title":"bug"}`))
	requireOutcome(t, outcome, err, true, "approval storage unavailable")
	if len(caller.calls) != 0 {
		t.Fatal("caller must not run without an approval decision")
	}
}

func TestMCPCallAllowRulePassesRealPolicy(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeBypass, "")
	scope, err := authority.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	// The executor sends no Parameters on the operation, so a saved rule's
	// parameter digest is the digest of a JSON null — the same value the
	// approval flow records for that operation shape.
	paramsDigest := sha256.Sum256([]byte("null"))
	rule := permission.ExactRule{
		Effect:           permission.EffectAllow,
		Kind:             permission.OpMCPTool,
		Name:             "github",
		Target:           "github__create_issue",
		ParametersDigest: hex.EncodeToString(paramsDigest[:]),
		ScopeDigest:      scope,
	}
	caller := newFakeMCPCaller()
	caller.output = "created"
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: policyGate{policy: permission.Policy{Rules: []permission.ExactRule{rule}}}, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp_call", `{"server":"github","tool":"create_issue"}`))
	requireOutcome(t, outcome, err, false, "created")
	if len(caller.calls) != 1 {
		t.Fatalf("caller calls = %d, want 1", len(caller.calls))
	}
}

func TestMCPCallResolvesTargetAndCoercesArguments(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	caller := newFakeMCPCaller()
	caller.output = "created"
	caller.schemas["github__create_issue"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":    map[string]any{"type": "string"},
			"labels":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"assignee": map[string]any{"type": "string"},
		},
	}
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, MCP: caller})

	arguments := `{"server":"github","tool":"create_issue","arguments":{"title":42,"labels":{"item":["bug","ui"]},"assignee":"neo"}}`
	outcome, err := runner.Execute(context.Background(), m06Call("mcp_call", arguments))
	requireOutcome(t, outcome, err, false, "created")
	if len(caller.calls) != 1 {
		t.Fatalf("caller calls = %d, want 1", len(caller.calls))
	}
	args := caller.calls[0].args
	if got := args["title"]; got != "42" {
		t.Fatalf("title = %#v, want \"42\"", got)
	}
	if got := args["labels"]; !reflect.DeepEqual(got, []any{"bug", "ui"}) {
		t.Fatalf("labels = %#v, want [bug ui]", got)
	}
	if got := args["assignee"]; got != "neo" {
		t.Fatalf("assignee = %#v, want neo", got)
	}
	// The gate sees the resolved target, not the raw mcp_call arguments.
	if gate.seen.Kind != permission.OpMCPTool || gate.seen.Name != "github" || gate.seen.Target != "github__create_issue" {
		t.Fatalf("gate operation = %#v", gate.seen)
	}
}

func TestMCPCallSkipsCoercionWithoutSchema(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	caller := newFakeMCPCaller()
	caller.output = "ok"
	// No schema registered for github__create_issue: arguments pass through.
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp_call", `{"server":"github","tool":"create_issue","arguments":{"count":3}}`))
	requireOutcome(t, outcome, err, false, "ok")
	if got := caller.calls[0].args["count"]; got != float64(3) {
		t.Fatalf("count = %#v, want 3 untouched", got)
	}
}

func TestMCPCallArgumentValidation(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	caller := newFakeMCPCaller()
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}, MCP: caller})

	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"missing target", `{}`, "server and tool are required"},
		{"arguments not an object", `{"server":"github","tool":"create_issue","arguments":"x"}`, "arguments must be an object"},
		{"unresolvable target", `{"server":"github","tool":"nope"}`, "unknown MCP tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := runner.Execute(context.Background(), m06Call("mcp_call", tc.arguments))
			requireOutcome(t, outcome, err, true, tc.want)
		})
	}
	// The guidance error lists the dispatch inventory.
	outcome, err := runner.Execute(context.Background(), m06Call("mcp_call", `{"server":"github","tool":"nope"}`))
	requireOutcome(t, outcome, err, true, "mcp__github__create_issue", "mcp__docs__search")
	if len(caller.calls) != 0 {
		t.Fatal("caller must not run for an unresolved target")
	}
}

func TestMCPDirectCallMalformedNameReturnsGuidance(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	caller := newFakeMCPCaller()
	runner := m06Runner(t, authority, ToolExecutorDeps{MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__nope", `{}`))
	requireOutcome(t, outcome, err, true, "unknown MCP tool", "available:")
	if len(caller.calls) != 0 {
		t.Fatal("caller must not run for an unknown tool name")
	}
}

func TestMCPDirectCallFlagsServerBusinessError(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	caller := newFakeMCPCaller()
	caller.output = "rate limited by the server"
	caller.isError = true
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{}`))
	requireOutcome(t, outcome, err, true, "rate limited by the server")
	if outcome.Status != agent.ToolFailed {
		t.Fatalf("outcome status = %q, want failed", outcome.Status)
	}
}

func TestMCPDirectCallTransportErrorSurfaces(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	caller := newFakeMCPCaller()
	caller.callErr = errors.New("connection refused")
	runner := m06Runner(t, authority, ToolExecutorDeps{Gate: gate, MCP: caller})

	outcome, err := runner.Execute(context.Background(), m06Call("mcp__github__create_issue", `{}`))
	requireOutcome(t, outcome, err, true, "mcp tool call failed", "connection refused")
	if outcome.Status != agent.ToolFailed {
		t.Fatalf("outcome status = %q, want failed", outcome.Status)
	}
}

func TestToolSearchFiltersAndCaps(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	caller := newFakeMCPCaller()
	runner := m06Runner(t, authority, ToolExecutorDeps{MCP: caller})

	// Empty query lists everything, capped at 20 with the remainder noted.
	broad := make([]MCPToolSchema, 0, 25)
	for i := 0; i < 25; i++ {
		broad = append(broad, MCPToolSchema{Name: fmt.Sprintf("mcp__bulk__tool_%02d", i), Description: "Bulk tool", InputSchema: map[string]any{"type": "object"}})
	}
	caller.dispatch = broad
	outcome, err := runner.Execute(context.Background(), m06Call("tool_search", `{}`))
	requireOutcome(t, outcome, err, false, "Found 25 dispatch tools", "5 more not listed", "mcp__bulk__tool_00")
	if strings.Contains(outcome.Content, "mcp__bulk__tool_20") {
		t.Fatal("tool_search listing exceeded the 20-entry cap")
	}

	// Keyword filtering matches names and descriptions, case-insensitively.
	caller.dispatch = fakeDispatchTools()
	outcome, err = runner.Execute(context.Background(), m06Call("tool_search", `{"query":"Issue"}`))
	requireOutcome(t, outcome, err, false, `Found 2 dispatch tools matching "issue"`, "mcp__github__create_issue", "mcp__github__list_issues")
	if strings.Contains(outcome.Content, "mcp__docs__search") {
		t.Fatal("non-matching tool listed")
	}

	// No match reports an explicit empty result.
	outcome, err = runner.Execute(context.Background(), m06Call("tool_search", `{"query":"kubernetes"}`))
	requireOutcome(t, outcome, err, false, "No dispatch tools match")

	// The limit argument lowers the cap but never raises it past 20.
	outcome, err = runner.Execute(context.Background(), m06Call("tool_search", `{"limit":1}`))
	requireOutcome(t, outcome, err, false, "Found 3 dispatch tools (showing 1, 2 more not listed", "mcp__github__create_issue")
	outcome, err = runner.Execute(context.Background(), m06Call("tool_search", `{"limit":99}`))
	requireOutcome(t, outcome, err, false, "Found 3 dispatch tools:")

	// An empty inventory is an explicit empty result too.
	empty := &fakeMCPCaller{}
	emptyRunner := m06Runner(t, authority, ToolExecutorDeps{MCP: empty})
	outcome, err = emptyRunner.Execute(context.Background(), m06Call("tool_search", `{}`))
	requireOutcome(t, outcome, err, false, "No dispatch tools are available")
}

func TestMCPEntriesWithoutCallerStayUnknown(t *testing.T) {
	formal := t.TempDir()
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	runner := m06Runner(t, authority, ToolExecutorDeps{})
	for _, name := range []string{"mcp__github__create_issue", "mcp_call", "tool_search"} {
		outcome, err := runner.Execute(context.Background(), m06Call(name, `{}`))
		requireOutcome(t, outcome, err, true, fmt.Sprintf("unknown tool %q", name))
	}
}

func TestNewToolExecutorFactoryInjectsMCPCaller(t *testing.T) {
	caller := newFakeMCPCaller()
	factory := NewToolExecutorFactory(ToolExecutorDeps{}, WithMCPCaller(caller)).(ToolExecutorFactory)
	if factory.deps.MCP == nil {
		t.Fatal("factory option must populate the MCP caller")
	}
	runner, err := factory.ForRun(executorRequest(t, t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if executor := runner.(*toolRunExecutor); executor.deps.MCP != caller {
		t.Fatal("run executor must share the injected MCP caller")
	}
}

func TestMCPCallInputIsRedactedInSessionlog(t *testing.T) {
	formal := t.TempDir()
	credential := "provider-secret-token-42"
	sessionDir := t.TempDir()
	info, err := sessionlog.Create(sessionDir, "mcp-redact")
	if err != nil {
		t.Fatal(err)
	}
	authority := m06Authority(t, formal, permission.ModeDefault, "")
	authority.SessionID = info.ID
	request := m06Request(t, authority)
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	caller := newFakeMCPCaller()
	caller.output = "created"
	factory := NewToolExecutorFactory(ToolExecutorDeps{Gate: gate, MCP: caller, SessionRoot: sessionDir, ProviderCredential: credential, Now: time.Now})
	runner, err := factory.ForRun(request)
	if err != nil {
		t.Fatal(err)
	}

	arguments := fmt.Sprintf(`{"server":"github","tool":"create_issue","arguments":{"title":"leak %s"}}`, credential)
	if _, err = runner.Execute(context.Background(), m06Call("mcp_call", arguments)); err != nil {
		t.Fatal(err)
	}
	replay, err := sessionlog.Replay(sessionDir, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	logged := 0
	for _, event := range replay.Events {
		raw, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if event.Type == sessionlog.EventToolCall {
			logged++
			if strings.Contains(string(raw), credential) {
				t.Fatalf("tool call event leaked the credential: %s", raw)
			}
			if !strings.Contains(string(raw), redact.Placeholder) {
				t.Fatalf("tool call event did not redact the credential: %s", raw)
			}
		}
	}
	if logged != 1 {
		t.Fatalf("tool call events = %d, want 1", logged)
	}
}
