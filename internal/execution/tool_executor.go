package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

const (
	toolOutputLimit = 50000
	toolRunTimeout  = 120 * time.Second
	commandTimeout  = 600 * time.Second
	defaultPoll     = 500 * time.Millisecond
)

// CandidateLifecycle is the existing persistence surface needed for lazy run candidates.
type CandidateLifecycle interface {
	SaveCandidate(context.Context, store.CandidateRecord) error
	TransitionCandidate(context.Context, string, string, string, string) error
}

type ToolExecutorDeps struct {
	Sandbox            sandbox.SandboxManager
	Gate               PermissionGate
	Approvals          permission.ApprovalRepository
	HelperPath         string
	SessionRoot        string
	Now                func() time.Time
	PollEvery          time.Duration
	Candidates         CandidateLifecycle
	ProviderCredential string
}

type ToolExecutorFactory struct{ deps ToolExecutorDeps }

func NewToolExecutorFactory(deps ToolExecutorDeps) agent.ExecutorFactory {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PollEvery <= 0 {
		deps.PollEvery = defaultPoll
	}
	return ToolExecutorFactory{deps: deps}
}

func (f ToolExecutorFactory) ForRun(request agent.ExecutionRequest) (agent.RunExecutor, error) {
	if request.RunID == "" || request.Work.SessionID == "" {
		return nil, errors.New("tool execution requires run and session IDs")
	}
	var authority permission.Authority
	if err := json.Unmarshal(request.PermissionBounds, &authority); err != nil {
		return nil, fmt.Errorf("decode trusted permission bounds: %w", err)
	}
	if authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID {
		return nil, errors.New("permission bounds do not match execution request")
	}
	if authority.AllowedRoot == "" || authority.CandidateRoot == "" {
		return nil, errors.New("permission bounds lack formal and candidate roots")
	}
	formal, err := filepath.Abs(authority.AllowedRoot)
	if err != nil {
		return nil, err
	}
	candidateRoot, err := filepath.Abs(authority.CandidateRoot)
	if err != nil {
		return nil, err
	}
	authority.AllowedRoot = filepath.Clean(formal)
	authority.CandidateRoot = filepath.Clean(candidateRoot)
	if authority.FormalRoot == "" {
		authority.FormalRoot = authority.AllowedRoot
	}
	runRoot := filepath.Join(filepath.Dir(authority.CandidateRoot), ".stable-runs", request.RunID)
	if err = os.MkdirAll(runRoot, 0700); err != nil {
		return nil, err
	}
	runRoot, err = filepath.Abs(runRoot)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(runRoot, 0700); err != nil {
		return nil, err
	}
	return &toolRunExecutor{deps: f.deps, request: request, authority: authority, runRoot: runRoot}, nil
}

type toolRunExecutor struct {
	deps             ToolExecutorDeps
	request          agent.ExecutionRequest
	authority        permission.Authority
	runRoot          string
	candidate        *candidate.Candidate
	approvalObserver func()
	readsMu          sync.Mutex
	reads            map[string]bool
}

// markRead records a workspace-relative path whose formal content the model
// has successfully read (or whose candidate content it has written) during
// this run.
func (e *toolRunExecutor) markRead(rel string) {
	e.readsMu.Lock()
	defer e.readsMu.Unlock()
	if e.reads == nil {
		e.reads = map[string]bool{}
	}
	e.reads[rel] = true
}

// hasRead reports whether the path was read (or written) earlier in this run.
func (e *toolRunExecutor) hasRead(rel string) bool {
	e.readsMu.Lock()
	defer e.readsMu.Unlock()
	return e.reads[rel]
}

func (e *toolRunExecutor) SetApprovalObserver(observer func()) {
	e.approvalObserver = observer
}

func (e *toolRunExecutor) Execute(ctx context.Context, call llm.ToolUse) (outcome agent.ToolOutcome, err error) {
	started := e.deps.Now()
	outcome = agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolFailed, IsError: true}
	args := map[string]any{}
	if len(call.Arguments) != 0 {
		if err = json.Unmarshal(call.Arguments, &args); err != nil {
			outcome.Content = "Error: invalid tool arguments"
			return outcome, nil
		}
	}
	if e.deps.SessionRoot != "" {
		if _, logErr := sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolCall, sessionlog.ToolCall{CallID: call.ID, Name: call.Name, Input: redactJSON(args, e.deps.ProviderCredential)}); logErr != nil {
			return outcome, fmt.Errorf("record tool call: %w", logErr)
		}
	}
	tool, kind, opName, rel, err := e.mapTool(call.Name, args)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return e.finish(outcome, started), nil
	}
	operation := permission.Operation{ID: call.ID, Kind: kind, Name: opName}
	if kind == permission.OpRead || kind == permission.OpWrite {
		root := e.authority.AllowedRoot
		if kind == permission.OpWrite {
			root = e.authority.CandidateRoot
		}
		operation.Target = filepath.Join(root, rel)
	}
	if call.Name == "command" {
		timeout := commandTimeoutFor(args)
		params, _ := json.Marshal(map[string]any{"command": args["command"], "timeout": timeout.String()})
		operation.Parameters = params
	}
	if e.deps.Gate == nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission gate unavailable"
		outcome.IsError = true
		return e.finish(outcome, started), nil
	}
	decision, authErr := e.deps.Gate.Authorize(ctx, e.authority, operation)
	if authErr != nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission authorization failed"
		return e.finish(outcome, started), nil
	}
	if decision.Kind == permission.DecisionAsk {
		if e.approvalObserver != nil {
			e.approvalObserver()
		}
		decision, authErr = e.waitForApproval(ctx, operation)
		if authErr != nil {
			if errors.Is(authErr, context.Canceled) || errors.Is(authErr, context.DeadlineExceeded) {
				return outcome, authErr
			}
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: approval unavailable"
			return e.finish(outcome, started), nil
		}
	}
	if decision.Kind != permission.DecisionAllow {
		outcome.Status = agent.ToolDenied
		outcome.Content = "Error: " + safeReason(decision.Reason, "operation denied")
		return e.finish(outcome, started), nil
	}
	if e.deps.Sandbox == nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: isolation unavailable"
		return e.finish(outcome, started), nil
	}
	if kind == permission.OpWrite || kind == permission.OpCommand {
		if err = e.ensureCandidate(ctx); err != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: candidate unavailable"
			return e.finish(outcome, started), nil
		}
	}
	if kind == permission.OpWrite {
		if err = e.checkMappedPath(kind, rel); err != nil {
			outcome.Content = "Error: " + err.Error()
			return e.finish(outcome, started), nil
		}
		// Read-before-write discipline lives here because helper processes are
		// per-call: overwriting a file that exists in the candidate copy
		// requires a successful read (or write) earlier in the same run.
		if !e.hasRead(rel) {
			if _, statErr := os.Lstat(filepath.Join(e.authority.CandidateRoot, rel)); statErr == nil {
				outcome.Content = "Error: file has not been read yet. Read it first before editing."
				return e.finish(outcome, started), nil
			}
		}
	}
	var response string
	var diff *agent.DiffSummary
	if call.Name == "command" {
		response, err = e.executeCommand(ctx, args)
	} else {
		response, diff, err = e.executeHelper(ctx, call.Name, tool, args, rel)
	}
	if err != nil {
		if errors.Is(err, sandbox.ErrUnavailable) {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: isolation unavailable; execution refused"
		} else if errors.Is(err, context.DeadlineExceeded) {
			outcome.Status, outcome.Content = agent.ToolTimeout, "Error: tool timed out"
		} else if errors.Is(err, context.Canceled) {
			return outcome, err
		} else {
			outcome.Status, outcome.Content = agent.ToolFailed, "Error: isolated tool execution failed"
		}
	} else {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, response
		if strings.HasPrefix(response, "Error:") {
			outcome.Status, outcome.IsError = agent.ToolFailed, true
		}
		outcome.Diff = diff
		if !outcome.IsError && (call.Name == "read_file" || call.Name == "write_file" || call.Name == "edit_file") {
			e.markRead(rel)
		}
	}
	return e.finish(outcome, started), nil
}

func (e *toolRunExecutor) mapTool(name string, args map[string]any) (helperName string, kind permission.OperationKind, opName, relative string, err error) {
	switch name {
	case "read_file":
		helperName, kind, opName = "ReadFile", permission.OpRead, "ReadFile"
		relative, err = cleanArg(args, "file_path", "")
	case "glob":
		helperName, kind, opName = "Glob", permission.OpRead, "Glob"
		relative, err = cleanArg(args, "path", ".")
	case "grep":
		helperName, kind, opName = "Grep", permission.OpRead, "Grep"
		relative, err = cleanArg(args, "path", ".")
	case "write_file":
		helperName, kind, opName = "WriteFile", permission.OpWrite, "WriteFile"
		relative, err = cleanArg(args, "file_path", "")
	case "edit_file":
		helperName, kind, opName = "EditFile", permission.OpWrite, "EditFile"
		relative, err = cleanArg(args, "file_path", "")
	case "command":
		if command, _ := args["command"].(string); strings.TrimSpace(command) == "" {
			return "", "", "", "", errors.New("command is required")
		}
		return "", permission.OpCommand, "Command", "", nil
	default:
		return "", "", "", "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", "", "", "", err
	}
	if relative == "." {
		relative = ""
	}
	if kind == permission.OpRead {
		if err = e.checkMappedPath(kind, relative); err != nil {
			return "", "", "", "", err
		}
	}
	if name == "glob" || name == "grep" {
		args["path"] = filepath.Join("/workspace/project", relative)
	} else if kind == permission.OpRead {
		args["file_path"] = filepath.Join("/workspace/project", relative)
	} else {
		args["file_path"] = filepath.Join("/workspace/candidate", relative)
	}
	return helperName, kind, opName, relative, nil
}

func cleanArg(args map[string]any, key, fallback string) (string, error) {
	value, ok := args[key].(string)
	if !ok || value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return candidate.CleanRelative(value)
}

func (e *toolRunExecutor) checkMappedPath(kind permission.OperationKind, rel string) error {
	root := e.authority.AllowedRoot
	if kind == permission.OpWrite {
		root = e.authority.CandidateRoot
		if e.candidate != nil {
			root = e.candidate.CandidateRoot
		}
	}
	path := filepath.Join(root, rel)
	canonical, err := pathWithoutSymlinkAncestors(path)
	if err != nil || canonical != path {
		return candidate.ErrUnsafePath
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || filepath.Clean(resolvedRoot) != filepath.Clean(root) {
		return candidate.ErrUnsafePath
	}
	if !pathWithinOrEqual(root, canonical) {
		return candidate.ErrUnsafePath
	}
	return nil
}

func (e *toolRunExecutor) ensureCandidate(ctx context.Context) error {
	if e.candidate != nil {
		return nil
	}
	parent := filepath.Dir(e.authority.CandidateRoot)
	id := filepath.Base(e.authority.CandidateRoot)
	created, err := candidate.CreateCandidate(id, e.authority.AllowedRoot, parent)
	if err != nil {
		return err
	}
	if e.deps.Candidates != nil {
		goalID := e.request.Work.GoalID
		if goalID == "" {
			goalID = "session-" + e.request.Work.SessionID
		}
		if err = e.deps.Candidates.SaveCandidate(ctx, store.CandidateRecord{
			Candidate: created,
			ActionID:  "tool-run-" + e.request.RunID,
			GoalID:    goalID,
		}); err != nil {
			_ = os.RemoveAll(created.CandidateRoot)
			return err
		}
		if err = e.deps.Candidates.TransitionCandidate(ctx, id, "prepared", "running", ""); err != nil {
			return err
		}
	}
	created.Status = "running"
	e.candidate = &created
	e.authority.CandidateRoot = created.CandidateRoot
	return nil
}

func (e *toolRunExecutor) executeHelper(ctx context.Context, modelName, helper string, args map[string]any, rel string) (string, *agent.DiffSummary, error) {
	if e.deps.HelperPath == "" {
		return "", nil, errors.New("tool helper is unavailable")
	}
	if err := e.checkMappedPath(permission.OpRead, filepath.Clean(rel)); err != nil && helper != "WriteFile" && helper != "EditFile" {
		return "", nil, err
	}
	workspace := "/workspace/project"
	if helper == "WriteFile" || helper == "EditFile" {
		workspace = "/workspace/candidate"
	}
	request, err := json.Marshal(HelperRequest{Tool: modelName, Args: args, Workspace: workspace})
	if err != nil {
		return "", nil, err
	}
	// Read-only helpers must work before any candidate exists (pure investigation
	// runs never create one), so the candidate mount slot is filled by a
	// per-call scratch directory the sandbox probe can write to. Read tools only
	// touch /workspace/project; nothing in the scratch survives the call.
	candidateMount := e.authority.CandidateRoot
	if helper == "ReadFile" || helper == "Glob" || helper == "Grep" {
		scratch, scratchErr := os.MkdirTemp(filepath.Dir(e.runRoot), "scratch-")
		if scratchErr != nil {
			return "", nil, scratchErr
		}
		defer os.RemoveAll(scratch)
		candidateMount = scratch
	}
	profile := sandbox.SandboxProfile{ProjectRoot: e.authority.AllowedRoot, CandidateRoot: candidateMount, RunRoot: e.runRoot, Timeout: toolRunTimeout, OutputLimit: 1 << 20}
	helperAbs, err := filepath.Abs(e.deps.HelperPath)
	if err != nil {
		return "", nil, err
	}
	profile.ReadOnlyMounts = []sandbox.ReadOnlyMount{{HostPath: filepath.Dir(helperAbs), GuestPath: "/workspace/runtime"}}
	result, err := e.deps.Sandbox.RunIsolated(ctx, profile, []string{"/workspace/runtime/" + filepath.Base(helperAbs), "--stable-tool-exec"}, bytes.NewReader(append(request, '\n')))
	if err != nil {
		return "", nil, err
	}
	if result.TimedOut {
		return "", nil, context.DeadlineExceeded
	}
	if result.ExitCode != 0 {
		return "", nil, fmt.Errorf("helper exited with status %d", result.ExitCode)
	}
	var response HelperResponse
	if err = json.Unmarshal(bytes.TrimSpace(result.Stdout), &response); err != nil {
		return "", nil, errors.New("helper returned invalid response")
	}
	output := strings.ReplaceAll(response.Output, e.authority.AllowedRoot, ".")
	output = strings.ReplaceAll(output, e.authority.CandidateRoot, ".")
	output = e.redact(output)
	var diff *agent.DiffSummary
	if response.Additions != 0 || response.Removals != 0 || response.DiffText != "" {
		diff = &agent.DiffSummary{Additions: response.Additions, Removals: response.Removals, Text: response.DiffText}
	}
	if response.IsError {
		return "Error: " + output, diff, nil
	}
	return output, diff, nil
}

func (e *toolRunExecutor) executeCommand(ctx context.Context, args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	if strings.ContainsRune(command, 0) {
		return "", errors.New("command contains NUL")
	}
	profile := sandbox.SandboxProfile{ProjectRoot: e.authority.AllowedRoot, CandidateRoot: e.authority.CandidateRoot, RunRoot: e.runRoot, Timeout: commandTimeoutFor(args), OutputLimit: 1 << 20}
	result, err := e.deps.Sandbox.RunIsolated(ctx, profile, []string{"bash", "-c", command}, nil)
	if err != nil {
		return "", err
	}
	if result.TimedOut {
		return "", context.DeadlineExceeded
	}
	output := strings.TrimSpace(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
	formatted := fmt.Sprintf("$ %s\n%s\nExit code %d", command, output, result.ExitCode)
	return e.redact(formatted), nil
}

func commandTimeoutFor(args map[string]any) time.Duration {
	seconds := 120
	switch value := args["timeout"].(type) {
	case float64:
		seconds = int(value)
	case int:
		seconds = value
	case int64:
		seconds = int(value)
	}
	if seconds <= 0 {
		seconds = 120
	}
	return min(time.Duration(seconds)*time.Second, commandTimeout)
}

func (e *toolRunExecutor) waitForApproval(ctx context.Context, operation permission.Operation) (permission.PermissionDecision, error) {
	if e.deps.Approvals == nil {
		return permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "approval storage unavailable"}, nil
	}
	digest, err := operation.Digest()
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	scope, err := e.authority.ScopeDigest()
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	ticker := time.NewTicker(e.deps.PollEvery)
	defer ticker.Stop()
	for {
		request, ok, getErr := e.deps.Approvals.GetApprovalForOperation(ctx, e.authority.RunID, digest, scope)
		if getErr != nil {
			return permission.PermissionDecision{}, getErr
		}
		if ok && request.Status != permission.ApprovalPending {
			return e.deps.Gate.Authorize(ctx, e.authority, operation)
		}
		if ok && !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(e.deps.Now()) {
			return permission.PermissionDecision{Kind: permission.DecisionDeny, Reason: "approval expired"}, nil
		}
		select {
		case <-ctx.Done():
			return permission.PermissionDecision{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *toolRunExecutor) finish(outcome agent.ToolOutcome, started time.Time) agent.ToolOutcome {
	outcome.Elapsed = e.deps.Now().Sub(started)
	if outcome.Elapsed < 0 {
		outcome.Elapsed = 0
	}
	outcome.Content = e.redact(outcome.Content)
	outcome.OutputBytes = len(outcome.Content)
	if len(outcome.Content) > toolOutputLimit {
		outcome.Content = outcome.Content[:toolOutputLimit] + "\n[output truncated]"
	}
	if e.deps.SessionRoot != "" && outcome.CallID != "" {
		errText := ""
		if outcome.IsError {
			errText = outcome.Content
		}
		_, _ = sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: outcome.CallID, Result: outcome.Content, Error: errText})
	}
	return outcome
}

func (e *toolRunExecutor) redact(value string) string {
	if e.deps.ProviderCredential != "" {
		value = strings.ReplaceAll(value, e.deps.ProviderCredential, "[credential redacted]")
	}
	return value
}

func redactJSON(value any, credential string) any {
	if credential == "" {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[input redacted]"
	}
	text := strings.ReplaceAll(string(encoded), credential, "[credential redacted]")
	var redacted any
	if json.Unmarshal([]byte(text), &redacted) != nil {
		return "[input redacted]"
	}
	return redacted
}

func safeReason(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

var _ agent.ExecutorFactory = ToolExecutorFactory{}
