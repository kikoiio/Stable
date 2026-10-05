package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/redact"
	"stable/internal/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/todo"
)

const (
	toolOutputLimit = 50000
	toolRunTimeout  = 120 * time.Second
	commandTimeout  = 600 * time.Second
	defaultPoll     = 500 * time.Millisecond
)

type toolRunExecutor struct {
	deps             ToolExecutorDeps
	request          agent.ExecutionRequest
	authority        permission.Authority
	runRoot          string
	candidate        *candidate.Candidate
	approvalObserver func()
	readsMu          sync.Mutex
	reads            map[string]bool
	// snapshotDigest is the candidate digest covered by the latest
	// checkpoint; an unchanged candidate reuses it instead of writing a
	// duplicate manifest. snapshotBlocked, once set, refuses every later
	// mutation in this run because a required checkpoint failed.
	snapshotDigest  string
	snapshotBlocked string
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
	args := map[string]any{}
	postHook := false
	defer func() {
		if postHook && ctx.Err() == nil && e.deps.HookRunner != nil {
			e.deps.HookRunner.PostToolUse(e.request.Work.SessionID, call.Name, args, outcome.Content)
		}
	}()
	outcome = agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolFailed, IsError: true}
	if len(call.Arguments) != 0 {
		if err = json.Unmarshal(call.Arguments, &args); err != nil {
			outcome.Content = "Error: invalid tool arguments"
			return e.finish(outcome, started), nil
		}
	}
	if e.deps.SessionRoot != "" {
		if _, logErr := sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolCall, sessionlog.ToolCall{CallID: call.ID, Name: call.Name, Input: redactJSON(args, e.deps.ProviderCredential)}); logErr != nil {
			return outcome, fmt.Errorf("record tool call: %w", logErr)
		}
	}
	if e.deps.HookRunner != nil {
		rejected, hookID, message := e.deps.HookRunner.PreToolUse(e.request.Work.SessionID, call.Name, args)
		if rejected {
			outcome.Status = agent.ToolDenied
			outcome.Content = fmt.Sprintf("Blocked by hook %s: %s", hookID, message)
			return e.finish(outcome, started), nil
		}
		postHook = true
	}
	// M06 host-side branches run before the sandbox mapping: the interactive
	// and task tools never touch the filesystem, and a write to the session's
	// plan file must bypass the candidate workspace and its checkpoints.
	if hostOutcome, handled := e.executeHostTool(ctx, call, args); handled {
		return e.finish(hostOutcome, started), nil
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
	// The permission gate resolves write targets against the candidate root,
	// so the workspace must exist before evaluation, not only after approval.
	// An unchanged candidate is cleaned up when the run finalizes, so a denied
	// or cancelled call still leaves no trace behind.
	if kind == permission.OpWrite || kind == permission.OpCommand {
		if err = e.ensureCandidate(ctx); err != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: candidate unavailable"
			return e.finish(outcome, started), nil
		}
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
				e.abortLoggedCall(call.ID, "Error: run cancelled while awaiting approval")
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
	// Checkpoint the candidate before any file-changing call. A blocked
	// candidate refuses further mutations, and a failed pre-state snapshot
	// blocks it instead of letting an uncheckpointed change through.
	mutating := kind == permission.OpWrite || kind == permission.OpCommand
	preDigest := ""
	if mutating {
		if e.snapshotBlocked != "" {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: candidate is blocked: "+e.snapshotBlocked
			return e.finish(outcome, started), nil
		}
		if e.deps.Snapshots != nil {
			if preDigest, err = e.preSnapshot(ctx, call.Name, &outcome); err != nil {
				outcome.Status, outcome.Content = agent.ToolFailed, "Error: pre-change snapshot failed; candidate is blocked from further writes and acceptance"
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
			e.abortLoggedCall(call.ID, "Error: run cancelled during execution")
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
		// After a successful mutation compare the manifest and checkpoint the
		// new state; a confirmed no-change call reuses the pre-state digest
		// and writes nothing. A failed post-state snapshot blocks the
		// candidate rather than faking a checkpoint.
		if mutating && !outcome.IsError && e.deps.Snapshots != nil {
			if postErr := e.postSnapshot(ctx, call.Name, preDigest, &outcome); postErr != nil {
				outcome.Status, outcome.IsError = agent.ToolFailed, true
				outcome.Content += "\nError: post-change snapshot failed; candidate is blocked from further writes and acceptance"
			}
		}
	}
	return e.finish(outcome, started), nil
}

// preSnapshot checkpoints the candidate before a file-changing tool call. A
// candidate unchanged since the latest checkpoint reuses that manifest
// instead of writing a duplicate, so only real state transitions consume the
// per-candidate manifest quota.
func (e *toolRunExecutor) preSnapshot(ctx context.Context, tool string, outcome *agent.ToolOutcome) (string, error) {
	_, digest, err := candidate.BuildManifest(e.candidate.CandidateRoot)
	if err != nil {
		e.blockCandidate(ctx, "pre-change snapshot failed")
		return "", err
	}
	if digest == e.snapshotDigest {
		return digest, nil
	}
	snap, err := e.deps.Snapshots.Create(e.request.Work.SessionID, e.candidate.ID, e.request.RunID, "pre:"+tool, e.candidate.CandidateRoot)
	if err != nil {
		e.blockCandidate(ctx, "pre-change snapshot failed")
		return "", err
	}
	e.snapshotDigest = snap.Digest
	outcome.Snapshots = append(outcome.Snapshots, snapshotMeta(snap))
	return digest, nil
}

// postSnapshot checkpoints the candidate after a successful file-changing
// call when its digest moved away from the pre-call state.
func (e *toolRunExecutor) postSnapshot(ctx context.Context, tool, preDigest string, outcome *agent.ToolOutcome) error {
	_, digest, err := candidate.BuildManifest(e.candidate.CandidateRoot)
	if err != nil {
		e.blockCandidate(ctx, "post-change snapshot failed")
		return err
	}
	if digest == preDigest {
		return nil
	}
	snap, err := e.deps.Snapshots.Create(e.request.Work.SessionID, e.candidate.ID, e.request.RunID, "post:"+tool, e.candidate.CandidateRoot)
	if err != nil {
		e.blockCandidate(ctx, "post-change snapshot failed")
		return err
	}
	e.snapshotDigest = snap.Digest
	outcome.Snapshots = append(outcome.Snapshots, snapshotMeta(snap))
	return nil
}

// blockCandidate refuses every later mutation in this run and flips the
// persisted candidate to blocked so it can neither be recreated by a later
// run nor frozen ready and accepted.
func (e *toolRunExecutor) blockCandidate(ctx context.Context, reason string) {
	e.snapshotBlocked = reason
	if e.deps.Candidates != nil && e.candidate != nil {
		_ = e.deps.Candidates.TransitionCandidate(ctx, e.candidate.ID, "running", "blocked", "")
	}
}

func snapshotMeta(snap candidate.FileSnapshot) agent.SnapshotMeta {
	return agent.SnapshotMeta{SnapshotID: snap.SnapshotID, CandidateID: snap.CandidateID, RunID: snap.RunID, Label: snap.Label, Digest: snap.Digest, CreatedAt: snap.CreatedAt}
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

// abortLoggedCall closes the sessionlog pairing for a call whose tool_call
// event was already recorded but which will never produce a real result
// because the run was cancelled. Without this the call id stays "pending"
// forever and sessionlog rejects any later reuse of the id ("duplicate
// pending tool call id"), poisoning the session for all future runs.
func (e *toolRunExecutor) abortLoggedCall(callID, reason string) {
	if e.deps.SessionRoot == "" || callID == "" {
		return
	}
	_, _ = sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: callID, Result: e.redact(reason), Error: e.redact(reason)})
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

// redact strips the provider credential from tool output before it reaches
// the model or the session log. A credential too short for redact.Redact to
// identify safely is still replaced literally: leaking it is worse than the
// mangling risk of a short replacement.
func (e *toolRunExecutor) redact(value string) string {
	redacted, err := redact.Redact(value, []string{e.deps.ProviderCredential})
	if err != nil {
		return strings.ReplaceAll(value, e.deps.ProviderCredential, redact.Placeholder)
	}
	return redacted
}

func redactJSON(value any, credential string) any {
	if credential == "" {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[input redacted]"
	}
	text, err := redact.Redact(string(encoded), []string{credential})
	if err != nil {
		text = strings.ReplaceAll(string(encoded), credential, redact.Placeholder)
	}
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

// ---------------------------------------------------------------------------
// M06 host-side tool branches
//
// ask_user, exit_plan_mode, the task_* family and the session plan-file
// direct write never reach the sandbox helper. They are dispatched before
// mapTool, which would otherwise redirect write targets into the candidate
// workspace, create a candidate and checkpoint it — all semantics the plan
// file and the interactive tools must not have.
// ---------------------------------------------------------------------------

// ask_user bounds, mirroring the M06 schema and the plan: 1-4 questions with
// 2-4 options each.
const (
	maxAskQuestions = 4
	minAskOptions   = 2
	maxAskOptions   = 4
)

// executeHostTool serves the M06 host-side branches listed above and reports
// handled=false for every tool the sandbox path should serve.
func (e *toolRunExecutor) executeHostTool(ctx context.Context, call llm.ToolUse, args map[string]any) (agent.ToolOutcome, bool) {
	outcome := agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolFailed, IsError: true}
	switch call.Name {
	case "ask_user":
		return e.executeAskUser(ctx, call, args, outcome), true
	case "exit_plan_mode":
		return e.executeExitPlanMode(ctx, call, outcome), true
	case "task_create", "task_get", "task_list", "task_update":
		return e.executeTaskTool(call, args, outcome), true
	case "load_skill":
		return e.executeLoadSkill(ctx, call, args, outcome), true
	case "write_file", "edit_file":
		if target, ok := e.planFileTarget(args); ok {
			return e.executePlanFileWrite(ctx, call, args, target, outcome), true
		}
	}
	return outcome, false
}

// executePlanFileWrite writes the session plan file on the host. The
// permission gate still arbitrates — in plan mode the policy allows exactly
// this one path, outside plan mode it denies the write like any other
// formal-root mutation. The write itself bypasses sandbox, candidate creation
// and snapshot checkpoints; content is credential-redacted before it is
// persisted and a credential too short to redact safely refuses the write.
func (e *toolRunExecutor) executePlanFileWrite(ctx context.Context, call llm.ToolUse, args map[string]any, target string, outcome agent.ToolOutcome) agent.ToolOutcome {
	if e.deps.Gate == nil {
		outcome.Content = "Error: permission gate unavailable"
		return outcome
	}
	operation := permission.Operation{ID: call.ID, Kind: permission.OpWrite, Name: "WriteFile", Target: target}
	if call.Name == "edit_file" {
		operation.Name = "EditFile"
	}
	decision, err := e.deps.Gate.Authorize(ctx, e.authority, operation)
	if err != nil {
		outcome.Content = "Error: permission authorization failed"
		return outcome
	}
	if decision.Kind != permission.DecisionAllow {
		outcome.Status = agent.ToolDenied
		outcome.Content = "Error: " + safeReason(decision.Reason, "operation denied")
		return outcome
	}
	var content string
	if call.Name == "write_file" {
		raw, ok := args["content"].(string)
		if !ok {
			outcome.Content = "Error: content is required"
			return outcome
		}
		content = raw
	} else {
		oldString, _ := args["old_string"].(string)
		newString, _ := args["new_string"].(string)
		if oldString == "" {
			outcome.Content = "Error: old_string is required"
			return outcome
		}
		previous, readErr := os.ReadFile(target)
		if readErr != nil {
			outcome.Content = "Error: plan file could not be read: " + readErr.Error()
			return outcome
		}
		switch count := strings.Count(string(previous), oldString); count {
		case 0:
			outcome.Content = "Error: old_string not found in plan file"
			return outcome
		case 1:
			content = strings.Replace(string(previous), oldString, newString, 1)
		default:
			outcome.Content = fmt.Sprintf("Error: old_string found %d times, must be unique", count)
			return outcome
		}
	}
	content, redactErr := redact.Redact(content, []string{e.deps.ProviderCredential})
	if redactErr != nil {
		outcome.Content = "Error: plan file content cannot be redacted safely; write refused"
		return outcome
	}
	if err = writePlanFileAtomic(target, []byte(content)); err != nil {
		outcome.Content = "Error: plan file write failed: " + err.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "计划文件已更新"
	return outcome
}

// planFileTarget reports whether a write_file/edit_file call targets the
// session's plan file and, if so, returns the resolved host path. Relative
// arguments are resolved against the authorized project root; the comparison
// uses the same resolution rules as the permission policy. An empty
// PlanFilePath or an unresolvable target is not a plan write — the sandbox
// path reports such errors.
func (e *toolRunExecutor) planFileTarget(args map[string]any) (string, bool) {
	if e.authority.PlanFilePath == "" {
		return "", false
	}
	raw, _ := args["file_path"].(string)
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	target, err := resolveHostPath(raw, e.authority.AllowedRoot)
	if err != nil {
		return "", false
	}
	planPath, err := resolveHostPath(e.authority.PlanFilePath, "")
	if err != nil {
		return "", false
	}
	return target, target == planPath
}

// resolveHostPath resolves a host-side path the way the permission policy
// does: absolute form, symbolic-link final components refused, otherwise
// evaluated through the nearest existing ancestor.
func resolveHostPath(path, base string) (string, error) {
	if base != "" && !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symbolic link targets are not accepted")
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		return filepath.Clean(resolved), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// writePlanFileAtomic replaces the plan file through a private temp file and
// a rename so a failed write never truncates the existing plan.
func writePlanFileAtomic(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("plan file must not be a symlink")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plan-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// executeAskUser validates the structured questions and blocks on the
// question sink until the user answers or the run context is cancelled. A
// cancellation is reported as a plain tool result — matching the source
// behaviour — so the model can react instead of the run aborting.
func (e *toolRunExecutor) executeAskUser(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	questions, err := parseAskQuestions(args)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	if e.deps.QuestionSink == nil {
		outcome.Content = "Error: 提问通道不可用"
		return outcome
	}
	response, askErr := e.deps.QuestionSink.Ask(ctx, AskRequest{
		SessionID: e.request.Work.SessionID,
		RunID:     e.request.RunID,
		WorkRef:   e.workRef(),
		Questions: questions,
	})
	if askErr != nil {
		if errors.Is(askErr, context.Canceled) || errors.Is(askErr, context.DeadlineExceeded) {
			outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "Question cancelled"
			return outcome
		}
		outcome.Content = "Error: " + askErr.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, formatAskAnswers(questions, response)
	return outcome
}

// workRef builds the host-facing work reference of the current run: goal runs
// always carry both GoalID and WorkItemID (the agent runner rejects one
// without the other), so the two identifiers are joined with "/"; session
// runs carry neither and use the session id as the reference instead.
func (e *toolRunExecutor) workRef() string {
	workRef := e.request.Work.GoalID
	if e.request.Work.WorkItemID != "" {
		if workRef != "" {
			workRef += "/"
		}
		workRef += e.request.Work.WorkItemID
	}
	if workRef == "" {
		workRef = "session-" + e.request.Work.SessionID
	}
	return workRef
}

// parseAskQuestions decodes and validates the ask_user questions argument.
func parseAskQuestions(args map[string]any) ([]QuestionSpec, error) {
	raw, ok := args["questions"].([]any)
	if !ok || len(raw) == 0 {
		return nil, errors.New("questions is required")
	}
	if len(raw) > maxAskQuestions {
		return nil, fmt.Errorf("at most %d questions are allowed", maxAskQuestions)
	}
	questions := make([]QuestionSpec, 0, len(raw))
	for i, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("question %d must be an object", i+1)
		}
		question, _ := entry["question"].(string)
		if strings.TrimSpace(question) == "" {
			return nil, fmt.Errorf("question %d is missing question", i+1)
		}
		header, _ := entry["header"].(string)
		if strings.TrimSpace(header) == "" {
			return nil, fmt.Errorf("question %d is missing header", i+1)
		}
		rawOptions, ok := entry["options"].([]any)
		if !ok || len(rawOptions) == 0 {
			return nil, fmt.Errorf("question %d is missing options", i+1)
		}
		if len(rawOptions) < minAskOptions || len(rawOptions) > maxAskOptions {
			return nil, fmt.Errorf("question %d must have between %d and %d options", i+1, minAskOptions, maxAskOptions)
		}
		options := make([]OptionSpec, 0, len(rawOptions))
		for j, rawOption := range rawOptions {
			option, ok := rawOption.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("question %d option %d must be an object", i+1, j+1)
			}
			label, _ := option["label"].(string)
			if strings.TrimSpace(label) == "" {
				return nil, fmt.Errorf("question %d option %d is missing label", i+1, j+1)
			}
			description, _ := option["description"].(string)
			options = append(options, OptionSpec{Label: label, Description: description})
		}
		multiSelect, _ := entry["multiSelect"].(bool)
		questions = append(questions, QuestionSpec{Question: question, Header: header, Options: options, MultiSelect: multiSelect})
	}
	return questions, nil
}

// formatAskAnswers renders the answers as one "Q:/A:" pair per question.
// A free-text response (the /reply or dialog overall answer) lists every
// question once and shows the single reply text as the shared answer.
func formatAskAnswers(questions []QuestionSpec, response AskResponse) string {
	if response.FreeText {
		text := "(no answer)"
		if len(response.Answers) > 0 && len(response.Answers[0]) > 0 && strings.TrimSpace(response.Answers[0][0]) != "" {
			text = response.Answers[0][0]
		}
		var b strings.Builder
		for i, question := range questions {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "Q: %s", question.Question)
		}
		fmt.Fprintf(&b, "\nA: %s", text)
		return b.String()
	}
	var b strings.Builder
	for i, question := range questions {
		answer := "(no answer)"
		if i < len(response.Answers) && len(response.Answers[i]) > 0 {
			answer = strings.Join(response.Answers[i], ", ")
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Q: %s\nA: %s", question.Question, answer)
	}
	return b.String()
}

// executeExitPlanMode presents the completed plan for user approval. The plan
// path comes from the authority: empty means the session is not in plan mode,
// a missing file means the plan was never written. Feedback and cancellation
// are decision outcomes reported as instruction text, not tool failures.
func (e *toolRunExecutor) executeExitPlanMode(ctx context.Context, call llm.ToolUse, outcome agent.ToolOutcome) agent.ToolOutcome {
	if e.authority.PlanFilePath == "" {
		outcome.Content = "Error: 当前不在计划模式"
		return outcome
	}
	if info, statErr := os.Lstat(e.authority.PlanFilePath); statErr != nil || info.Mode()&os.ModeSymlink != 0 {
		outcome.Content = "Error: 计划文件尚未创建"
		return outcome
	}
	if e.deps.PlanSink == nil {
		outcome.Content = "Error: 审批通道不可用"
		return outcome
	}
	// Both approved choices ("auto"/"manual") only differ in how the host
	// schedules later runs; the tool result is the same end-turn instruction.
	_, err := e.deps.PlanSink.SubmitPlan(ctx, e.request.Work.SessionID, e.request.RunID, e.authority.PlanFilePath)
	if err == nil {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "计划已批准,请结束本回合,不要再调用任何工具"
		return outcome
	}
	var feedback PlanFeedbackError
	if errors.As(err, &feedback) {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "用户要求继续修改计划:"+feedback.Text+",请保持计划模式"
		return outcome
	}
	var cancelled PlanCancelledError
	if errors.As(err, &cancelled) {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "计划审批已取消"
		return outcome
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "Question cancelled"
		return outcome
	}
	outcome.Content = "Error: " + err.Error()
	return outcome
}

// executeTaskTool serves the task_* family from the per-session task list.
// Task operations are host state changes with no file or sandbox semantics.
func (e *toolRunExecutor) executeTaskTool(call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	if e.deps.TodoProvider == nil {
		outcome.Content = "Error: 任务清单通道不可用"
		return outcome
	}
	list := e.deps.TodoProvider.For(e.request.Work.SessionID)
	if list == nil {
		outcome.Content = "Error: 任务清单通道不可用"
		return outcome
	}
	switch call.Name {
	case "task_create":
		return e.executeTaskCreate(list, args, outcome)
	case "task_get":
		return e.executeTaskGet(list, args, outcome)
	case "task_list":
		return e.executeTaskListOp(list, outcome)
	case "task_update":
		return e.executeTaskUpdate(list, args, outcome)
	}
	outcome.Content = fmt.Sprintf("Error: unknown tool %q", call.Name)
	return outcome
}

// executeLoadSkill activates a skill through the host SkillProvider and
// returns its rendered body as the tool result. The provider composes all
// error cases — unknown skill (with the available names), fork-mode skills,
// unreadable bodies — so the executor only validates the arguments.
func (e *toolRunExecutor) executeLoadSkill(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		outcome.Content = "Error: name is required"
		return outcome
	}
	if e.deps.SkillProvider == nil {
		outcome.Content = "Error: 技能通道不可用"
		return outcome
	}
	skillArgs, _ := args["args"].(string)
	body, err := e.deps.SkillProvider.LoadSkill(ctx, e.request.Work.SessionID, name, skillArgs)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "# Skill: "+name+"\n\n"+body
	return outcome
}

func (e *toolRunExecutor) executeTaskCreate(list *todo.TaskList, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	subject, _ := args["subject"].(string)
	description, _ := args["description"].(string)
	if strings.TrimSpace(subject) == "" {
		outcome.Content = "Error: subject is required"
		return outcome
	}
	if strings.TrimSpace(description) == "" {
		outcome.Content = "Error: description is required"
		return outcome
	}
	activeForm, _ := args["activeForm"].(string)
	metadata, err := stringMapArg(args["metadata"])
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	task, createErr := list.Create(subject, description, activeForm, metadata)
	if createErr != nil {
		outcome.Content = "Error: " + createErr.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "Created task:\n"+renderTask(task)
	return outcome
}

func (e *toolRunExecutor) executeTaskGet(list *todo.TaskList, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	id, err := taskIDArg(args)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	task, getErr := list.Get(id)
	if getErr != nil {
		outcome.Content = "Error: " + getErr.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, renderTask(task)
	return outcome
}

func (e *toolRunExecutor) executeTaskListOp(list *todo.TaskList, outcome agent.ToolOutcome) agent.ToolOutcome {
	tasks, err := list.List()
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	if len(tasks) == 0 {
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "No tasks."
		return outcome
	}
	lines := make([]string, 0, len(tasks))
	for _, task := range tasks {
		marker := "  "
		if task.Status == todo.StatusInProgress {
			marker = "* "
		}
		lines = append(lines, fmt.Sprintf("%s%s [%s] %s", marker, task.ID, task.Status, task.Subject))
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, strings.Join(lines, "\n")
	return outcome
}

func (e *toolRunExecutor) executeTaskUpdate(list *todo.TaskList, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	id, err := taskIDArg(args)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	patch := todo.UpdatePatch{}
	if value, ok := args["subject"].(string); ok {
		patch.Subject = &value
	}
	if value, ok := args["description"].(string); ok {
		patch.Description = &value
	}
	if value, ok := args["activeForm"].(string); ok {
		patch.ActiveForm = &value
	}
	if value, ok := args["owner"].(string); ok {
		patch.Owner = &value
	}
	if value, ok := args["status"].(string); ok {
		status := todo.Status(value)
		patch.Status = &status
	}
	blocks, err := stringSliceArg(args["addBlocks"])
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	patch.AddBlocks = blocks
	blockedBy, err := stringSliceArg(args["addBlockedBy"])
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	patch.AddBlockedBy = blockedBy
	metadata, err := stringMapArg(args["metadata"])
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	if metadata != nil {
		patch.Metadata = &metadata
	}
	task, updateErr := list.Update(id, patch)
	if updateErr != nil {
		outcome.Content = "Error: " + updateErr.Error()
		return outcome
	}
	content := "Updated task:\n" + renderTask(task)
	if patch.Status != nil && *patch.Status == todo.StatusDeleted {
		content = "Deleted task " + task.ID
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, content
	return outcome
}

// taskIDArg reads the required taskId argument.
func taskIDArg(args map[string]any) (string, error) {
	id, _ := args["taskId"].(string)
	if strings.TrimSpace(id) == "" {
		return "", errors.New("taskId is required")
	}
	return id, nil
}

// stringSliceArg reads an optional array-of-strings argument; a missing key
// returns nil without error, a wrong element type fails the call.
func stringSliceArg(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("expected an array of strings")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(string)
		if !ok {
			return nil, errors.New("expected an array of strings")
		}
		out = append(out, entry)
	}
	return out, nil
}

// stringMapArg reads an optional string-to-string map argument; a missing key
// returns nil without error, a wrong value type fails the call.
func stringMapArg(value any) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("expected an object with string values")
	}
	out := make(map[string]string, len(fields))
	for key, item := range fields {
		entry, ok := item.(string)
		if !ok {
			return nil, errors.New("expected an object with string values")
		}
		out[key] = entry
	}
	return out, nil
}

// renderTask renders one task as compact text for the model.
func renderTask(task todo.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s", task.ID, task.Status, task.Subject)
	if task.Description != "" {
		fmt.Fprintf(&b, "\n  description: %s", task.Description)
	}
	if task.ActiveForm != "" {
		fmt.Fprintf(&b, "\n  active form: %s", task.ActiveForm)
	}
	if task.Owner != "" {
		fmt.Fprintf(&b, "\n  owner: %s", task.Owner)
	}
	if len(task.Blocks) != 0 {
		fmt.Fprintf(&b, "\n  blocks: %s", strings.Join(task.Blocks, ", "))
	}
	if len(task.BlockedBy) != 0 {
		fmt.Fprintf(&b, "\n  blocked by: %s", strings.Join(task.BlockedBy, ", "))
	}
	if len(task.Metadata) != 0 {
		keys := make([]string, 0, len(task.Metadata))
		for key := range task.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+task.Metadata[key])
		}
		fmt.Fprintf(&b, "\n  metadata: %s", strings.Join(pairs, ", "))
	}
	return b.String()
}

var _ agent.ExecutorFactory = ToolExecutorFactory{}
