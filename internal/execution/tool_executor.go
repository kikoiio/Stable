package execution

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"stable/internal/platform/secfile"
	"strconv"
	"strings"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/proc"
	"stable/internal/platform/sandbox"
	"stable/internal/redact"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/todo"
	"stable/internal/workspace"
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
	var writeReservation workspace.WriteReservation
	reservationSettled := false
	defer func() {
		if writeReservation != nil && !reservationSettled {
			writeReservation.Release()
		}
	}()
	defer func() {
		if postHook && ctx.Err() == nil && e.deps.HookRunner != nil {
			e.deps.HookRunner.PostToolUseRun(ctx, e.hookParentRun(), e.request.Work.SessionID, call.Name, args, outcome.Content)
		}
	}()
	outcome = agent.ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: agent.ToolFailed, IsError: true}
	if e.deps.ReadOnly && call.Name != "read_file" && call.Name != "glob" && call.Name != "grep" {
		outcome.Content = "Error: child run only permits read_file, glob, and grep"
		return e.finish(outcome, started), nil
	}
	workspaceLifecycleAllowed := e.deps.WorkspaceLifecycle != nil && isWorkspaceLifecycleTool(call.Name) && e.request.TeamTurn == nil && !e.request.TeamCoordinator && !e.request.TeamUser
	if e.deps.WorkspaceLease != nil && call.Name != "read_file" && call.Name != "glob" && call.Name != "grep" && call.Name != "write_file" && call.Name != "edit_file" && call.Name != "command" && !workspaceLifecycleAllowed {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace child only permits bounded file tools and isolated command"
		return e.finish(outcome, started), nil
	}
	if e.deps.WorkspaceLease != nil && !isWorkspaceLifecycleTool(call.Name) {
		leaseCheck, leaseErr := e.deps.WorkspaceAccounting.ReserveWriterWrite(ctx, *e.deps.WorkspaceLease, 0)
		if leaseErr != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace writer lease is stale or blocked"
			return e.finish(outcome, started), nil
		}
		leaseCheck.Release()
	}
	if len(call.Arguments) != 0 {
		if err = json.Unmarshal(call.Arguments, &args); err != nil {
			outcome.Content = "Error: invalid tool arguments"
			return e.finish(outcome, started), nil
		}
	}
	if e.authority.ReadOnly && call.Name != "read_file" && call.Name != "glob" && call.Name != "grep" {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: tool is not allowed in a read-only session"
		return e.finish(outcome, started), nil
	}
	if e.deps.SessionRoot != "" {
		if _, logErr := sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolCall, sessionlog.ToolCall{CallID: call.ID, RunID: e.request.RunID, Name: call.Name, Input: redactJSON(memoryAuditInput(call.Name, args), e.deps.ProviderCredential)}); logErr != nil {
			return outcome, fmt.Errorf("record tool call: %w", logErr)
		}
	}
	if e.request.TeamCoordinator && !TeamCoordinatorToolAllowed(call.Name) {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: coordinator mode only permits team coordination tools"
		return e.finish(outcome, started), nil
	}
	if isWorkspaceLifecycleTool(call.Name) {
		var allowed bool
		outcome, allowed = e.authorizeWorkspaceLifecycleTool(ctx, call, args, outcome)
		if !allowed {
			return e.finish(outcome, started), nil
		}
	}
	if !e.authority.ReadOnly && e.deps.HookRunner != nil {
		rejected, hookID, message := e.deps.HookRunner.PreToolUseRun(ctx, e.hookParentRun(), e.request.Work.SessionID, call.Name, args)
		if rejected {
			outcome.Status = agent.ToolDenied
			outcome.Content = fmt.Sprintf("Blocked by hook %s: %s", hookID, message)
			return e.finish(outcome, started), nil
		}
		postHook = true
	}
	if isWorkspaceLifecycleTool(call.Name) {
		return e.finish(e.executeWorkspaceLifecycleTool(ctx, call, outcome), started), nil
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
		if e.authority.ReadOnly {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: read operation requires interactive approval"
			return e.finish(outcome, started), nil
		}
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
		if e.deps.WorkspaceLease != nil {
			growth, estimateErr := e.estimateWorkspaceWrite(args, call.Name, rel)
			if estimateErr != nil {
				outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace write exceeds file limits"
				return e.finish(outcome, started), nil
			}
			writeReservation, err = e.deps.WorkspaceAccounting.ReserveWriterWrite(ctx, *e.deps.WorkspaceLease, growth)
			if err != nil {
				outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace quota or writer lease rejected the change"
				return e.finish(outcome, started), nil
			}
		}
	}
	if kind == permission.OpCommand && e.deps.WorkspaceLease != nil {
		used, usageErr := workspace.DiskUsage(ctx, e.deps.WorkspaceLease.Paths.Root, workspace.DefaultLimits())
		if usageErr != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace usage unavailable"
			return e.finish(outcome, started), nil
		}
		writeReservation, err = e.deps.WorkspaceAccounting.ReserveWriterWrite(ctx, *e.deps.WorkspaceLease, workspace.DefaultLimits().MaxWorkspaceBytes-used)
		if err != nil {
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace command budget unavailable"
			return e.finish(outcome, started), nil
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

	if writeReservation != nil {
		settleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		commitErr := writeReservation.Commit(settleCtx)
		cancel()
		reservationSettled = true
		if commitErr != nil {
			outcome.Status, outcome.Content = agent.ToolFailed, "Error: workspace write settlement failed; writer is blocked"
			return e.finish(outcome, started), err
		}
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
	_, digest, err := candidate.BuildManifestForPolicy(e.candidate.CandidateRoot, e.candidate.ManifestPolicy)
	if err != nil {
		e.blockCandidate(ctx, "pre-change snapshot failed")
		return "", err
	}
	if digest == e.snapshotDigest {
		return digest, nil
	}
	snap, err := e.deps.Snapshots.CreateForPolicy(e.request.Work.SessionID, e.candidate.ID, e.request.RunID, "pre:"+tool, e.candidate.CandidateRoot, e.candidate.ManifestPolicy)
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
	_, digest, err := candidate.BuildManifestForPolicy(e.candidate.CandidateRoot, e.candidate.ManifestPolicy)
	if err != nil {
		e.blockCandidate(ctx, "post-change snapshot failed")
		return err
	}
	if digest == preDigest {
		return nil
	}
	snap, err := e.deps.Snapshots.CreateForPolicy(e.request.Work.SessionID, e.candidate.ID, e.request.RunID, "post:"+tool, e.candidate.CandidateRoot, e.candidate.ManifestPolicy)
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
		if e.deps.WorkspaceLease != nil {
			lease := e.deps.WorkspaceLease
			if err := sandbox.BoundedWorkspaceVolume(lease.Paths.Root, lease.Paths.Baseline, lease.Paths.Repository, lease.Paths.Checkout, lease.Paths.Run); err != nil {
				return "", "", "", "", err
			}
		}
		return "", permission.OpCommand, "Command", "", nil
	default:
		return "", "", "", "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", "", "", "", err
	}
	if e.deps.WorkspaceLease != nil {
		if name == "command" {
			return "", "", "", "", errors.New("command requires an enforced workspace disk quota")
		}
		if relative != "" && workspace.ProtectedRoot(relative) {
			return "", "", "", "", workspace.ErrUnsafePath
		}
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
	if e.deps.WorkspaceLease != nil {
		root = e.authority.CandidateRoot
	}
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
	if e.deps.WorkspaceLease != nil {
		lease := e.deps.WorkspaceLease
		if lease.RunID != e.request.RunID || lease.Generation == 0 || filepath.Clean(lease.Paths.Checkout) != filepath.Clean(e.authority.CandidateRoot) {
			return workspace.ErrOwnership
		}
		for _, root := range []string{lease.Paths.Baseline, lease.Paths.Checkout, lease.Paths.Run} {
			info, err := os.Lstat(root)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return workspace.ErrOwnership
			}
		}
		return nil
	}
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

func (e *toolRunExecutor) estimateWorkspaceWrite(args map[string]any, name, rel string) (int64, error) {
	var size int
	switch name {
	case "write_file":
		content, ok := args["content"].(string)
		if !ok {
			return 0, workspace.ErrOwnership
		}
		size = len(content)
	case "edit_file":
		content, ok := args["new_string"].(string)
		if !ok {
			return 0, workspace.ErrOwnership
		}
		old, ok := args["old_string"].(string)
		if !ok || old == "" {
			return 0, workspace.ErrOwnership
		}
		info, err := os.Lstat(filepath.Join(e.authority.CandidateRoot, rel))
		if err != nil || !info.Mode().IsRegular() || info.Size() > workspace.DefaultLimits().MaxFileBytes {
			return 0, workspace.ErrQuota
		}
		data, err := os.ReadFile(filepath.Join(e.authority.CandidateRoot, rel))
		if err != nil {
			return 0, err
		}
		replacements := strings.Count(string(data), old)
		if all, _ := args["replace_all"].(bool); !all && replacements > 1 {
			replacements = 1
		}
		size = len(data) + replacements*(len(content)-len(old))
	default:
		return 0, workspace.ErrOwnership
	}
	limits := workspace.DefaultLimits()
	if int64(size) > limits.MaxFileBytes || int64(size) > limits.MaxWorkspaceBytes-(64<<10) {
		return 0, workspace.ErrQuota
	}
	return int64(size) + (64 << 10), nil
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
	helperAbs, err := filepath.Abs(e.deps.HelperPath)
	if err != nil {
		return "", nil, err
	}
	// Build every one-shot helper profile through the trusted authority path.
	// Read-only helpers retain their per-call scratch candidate by narrowing the
	// copied authority before profile construction; the original authority and
	// persistent session bounds remain unchanged.
	profileAuthority := e.authority
	profileAuthority.CandidateRoot = candidateMount
	if e.deps.WorkspaceLease != nil && (helper == "ReadFile" || helper == "Glob" || helper == "Grep") {
		profileAuthority.AllowedRoot = e.authority.CandidateRoot
	}
	profile, err := OneShotSandboxProfile(ctx, profileAuthority, e.runRoot, helperAbs, toolRunTimeout, 1<<20, nil)
	if err != nil {
		return "", nil, err
	}
	profile.ReadOnlyFiles = []sandbox.ReadOnlyFileMount{{HostPath: helperAbs, GuestPath: "/workspace/runtime/agentworker"}}
	profile.WorkspaceIsolation = e.deps.WorkspaceLease != nil
	result, err := e.deps.Sandbox.RunIsolated(ctx, profile, []string{"/workspace/runtime/agentworker", "--stable-tool-exec"}, bytes.NewReader(append(request, '\n')))
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
	helperPath := ""
	if e.deps.HelperPath != "" {
		var err error
		helperPath, err = filepath.Abs(e.deps.HelperPath)
		if err != nil {
			return "", err
		}
	}
	timeout := commandTimeoutFor(args)
	if e.deps.WorkspaceLease != nil {
		timeout = min(timeout, 90*time.Second)
	}
	profile, err := OneShotSandboxProfile(ctx, e.authority, e.runRoot, helperPath, timeout, 1<<20, nil)
	if err != nil {
		return "", err
	}
	if e.deps.WorkspaceLease != nil {
		accounting, ok := e.deps.WorkspaceAccounting.(workspace.WriterProcessAccounting)
		if !ok {
			return "", errors.New("workspace command requires durable sandbox process identity tracking")
		}
		var tokenBytes [32]byte
		if _, err := rand.Read(tokenBytes[:]); err != nil {
			return "", fmt.Errorf("create workspace process token: %w", err)
		}
		identity := proc.TrackedProcess{
			Token: hex.EncodeToString(tokenBytes[:]), WorkspaceID: e.deps.WorkspaceLease.WorkspaceID,
			RunID: e.deps.WorkspaceLease.RunID, Generation: e.deps.WorkspaceLease.Generation,
		}
		profile.WorkspaceIsolation = true
		profile.WorkspaceVolumeRoot = e.deps.WorkspaceLease.Paths.Root
		profile.WorkspaceProcess = &identity
		lease := *e.deps.WorkspaceLease
		profile.OnProcessStart = func(process proc.TrackedProcess) error {
			return accounting.RegisterWriterProcess(context.Background(), lease, process)
		}
		profile.OnProcessExit = func(process proc.TrackedProcess) error {
			return accounting.ClearWriterProcess(context.Background(), lease, process)
		}
	}
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
		loggedResult := outcome.Content
		if outcome.ToolName == "memory_read" {
			loggedResult = "[memory content omitted]"
		}
		if outcome.IsError {
			errText = loggedResult
		}
		_, _ = sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventToolResult, sessionlog.ToolResult{CallID: outcome.CallID, Result: loggedResult, Error: errText})
	}
	return outcome
}

func memoryAuditInput(name string, args map[string]any) map[string]any {
	if name != "memory_save" {
		return args
	}
	copy := make(map[string]any, len(args))
	for key, value := range args {
		if key != "body" {
			copy[key] = value
		}
	}
	copy["body"] = "[memory content omitted]"
	return copy
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

// executeHostTool serves the M06 host-side branches and the M07-C MCP entries
// listed above and reports handled=false for every tool the sandbox path
// should serve. The MCP entries are wired only when an MCPCaller is injected;
// otherwise they fall through so mapTool reports the plain unknown-tool error.
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
	case "run_agent", "task_output", "task_stop":
		return e.executeAgentTaskTool(ctx, call, args, outcome), true
	case "team_create", "team_member_spawn", "team_member_resume", "team_list", "team_get", "team_close", "team_member_get", "team_member_list", "team_send", "team_messages", "team_plan_submit", "team_request_list", "team_request_respond", "team_shutdown_request", "team_task_create", "team_task_get", "team_task_list", "team_task_update":
		if e.deps.TeamTools != nil {
			teamOutcome, err := e.deps.TeamTools.Execute(ctx, e.request, call)
			if err != nil {
				outcome.Status, outcome.Content = agent.ToolFailed, "Error: team operation failed"
				return outcome, true
			}
			return teamOutcome, true
		}
	case "mcp_call":
		if e.deps.MCP != nil {
			return e.executeMCPCall(ctx, call, args, outcome), true
		}
	case "tool_search":
		if e.deps.MCP != nil {
			return e.executeToolSearch(args, outcome), true
		}
	case "memory_list", "memory_read", "memory_save", "memory_delete":
		if e.deps.Memory != nil {
			content, operation, scope, entry, err := e.executeMemoryTool(ctx, call.Name, args)
			state := "success"
			if err != nil {
				state = "failure"
				content = "Error: " + err.Error()
			}
			if e.deps.SessionRoot != "" {
				_, _ = sessionlog.Append(e.deps.SessionRoot, e.request.Work.SessionID, sessionlog.EventMemoryAction, sessionlog.MemoryActionRecord{
					Scope: scope, Entry: boundedMemoryEventText(entry), Operation: operation, State: state, RunID: e.request.RunID, At: memoryActionTime(e.deps.Now),
				})
			}
			outcome.Content = content
			if err != nil {
				return outcome, true
			}
			outcome.Status, outcome.IsError = agent.ToolSucceeded, false
			return outcome, true
		}
	case "delegate_tasks":
		if e.deps.Delegator != nil {
			return e.executeDelegation(ctx, args, outcome), true
		}
	case "write_file", "edit_file":
		if target, ok := e.planFileTarget(args); ok {
			return e.executePlanFileWrite(ctx, call, args, target, outcome), true
		}
	default:
		if strings.HasPrefix(call.Name, "mcp__") && e.deps.MCP != nil {
			return e.executeMCPDirectCall(ctx, call, args, outcome), true
		}
	}
	return outcome, false
}

func boundedMemoryEventText(value string) string {
	runes := []rune(value)
	if len(runes) > sessionlog.MaxMemoryEventText {
		return string(runes[:sessionlog.MaxMemoryEventText])
	}
	return value
}

func (e *toolRunExecutor) authorizeWorkspaceLifecycleTool(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) (agent.ToolOutcome, bool) {
	if e.deps.WorkspaceLifecycle == nil || e.request.TeamTurn != nil || e.request.TeamCoordinator || e.request.TeamUser {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace lifecycle tools are available only to a lead run"
		return outcome, false
	}
	if e.deps.Gate == nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission gate unavailable"
		return outcome, false
	}
	parameters, err := json.Marshal(args)
	if err != nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace lifecycle arguments are invalid"
		return outcome, false
	}
	operation := permission.Operation{ID: call.ID, Kind: permission.OpWorkspaceLifecycle, Name: call.Name, Parameters: parameters}
	decision, authErr := e.deps.Gate.Authorize(ctx, e.authority, operation)
	if authErr != nil {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: permission authorization failed"
		return outcome, false
	}
	if decision.Kind == permission.DecisionAsk {
		if e.approvalObserver != nil {
			e.approvalObserver()
		}
		decision, authErr = e.waitForApproval(ctx, operation)
		if authErr != nil {
			if errors.Is(authErr, context.Canceled) || errors.Is(authErr, context.DeadlineExceeded) {
				outcome.Content = "Error: run cancelled while awaiting workspace permission approval"
				return outcome, false
			}
			outcome.Status, outcome.Content = agent.ToolDenied, "Error: workspace permission approval unavailable"
			return outcome, false
		}
	}
	if decision.Kind != permission.DecisionAllow {
		outcome.Status, outcome.Content = agent.ToolDenied, "Error: "+safeReason(decision.Reason, "workspace lifecycle operation denied")
		return outcome, false
	}
	return outcome, true
}

func (e *toolRunExecutor) executeWorkspaceLifecycleTool(ctx context.Context, call llm.ToolUse, outcome agent.ToolOutcome) agent.ToolOutcome {
	var lease *workspace.WriterLease
	if e.deps.WorkspaceLease != nil {
		leaseCopy := *e.deps.WorkspaceLease
		lease = &leaseCopy
	}
	result, err := e.deps.WorkspaceLifecycle.Execute(ctx, e.request, lease, call)
	if err != nil {
		outcome.Status, outcome.Content = agent.ToolFailed, "Error: workspace lifecycle operation unavailable"
		return outcome
	}
	if result.CallID == "" {
		result.CallID = call.ID
	}
	if result.ToolName == "" {
		result.ToolName = call.Name
	}
	return result
}

func (e *toolRunExecutor) executeDelegation(ctx context.Context, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	if e.request.Work.Kind != agent.WorkSession {
		outcome.Content = "Error: delegate_tasks is available only to session runs"
		return outcome
	}
	if e.deps.Provider == nil || e.deps.Delegator == nil {
		outcome.Content = "Error: delegation service is unavailable"
		return outcome
	}
	rawTasks, ok := args["tasks"].([]any)
	if !ok {
		outcome.Content = "Error: tasks must be an array"
		return outcome
	}
	tasks := make([]agent.DelegationTask, 0, len(rawTasks))
	for i, raw := range rawTasks {
		item, ok := raw.(map[string]any)
		if !ok {
			outcome.Content = fmt.Sprintf("Error: task %d must be an object", i+1)
			return outcome
		}
		task := agent.DelegationTask{}
		task.ID, _ = item["id"].(string)
		task.Name, _ = item["name"].(string)
		task.Instruction, _ = item["instruction"].(string)
		tasks = append(tasks, task)
	}
	childDeps := e.deps
	childDeps.SessionRoot = "" // child tool calls are intentionally not transcript events
	childDeps.HookRunner = nil // read-only child tools cannot recursively invoke hooks
	childFactory := NewToolExecutorFactory(childDeps, WithReadOnlyTools())
	permissionBounds, err := json.Marshal(e.authority)
	if err != nil {
		outcome.Content = "Error: could not derive delegation permissions"
		return outcome
	}
	parent := agent.ParentRun{
		RunID: e.request.RunID, Deadline: e.request.RunDeadline, Work: e.request.Work,
		ProjectRoot: e.authority.AllowedRoot, PermissionBounds: permissionBounds,
		Provider: e.deps.Provider, ProviderName: e.request.ProviderName, Model: e.request.Model,
		ToolSchemas: ReadOnlyToolSchemas(), ExecutorFactory: childFactory,
	}
	results, err := e.deps.Delegator.RunBatch(ctx, parent, tasks)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		outcome.Content = "Error: could not encode delegation results"
		return outcome
	}
	outcome.Content = string(encoded)
	outcome.Status = agent.ToolSucceeded
	outcome.IsError = true
	for _, result := range results {
		if result.Status == agent.DelegationSucceeded {
			outcome.IsError = false
			break
		}
	}
	if outcome.IsError {
		outcome.Status = agent.ToolFailed
	}
	return outcome
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
	if err = secfile.ChmodPrivate(tmp.Name(), 0600); err != nil {
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
// returns its rendered body or fork result as the tool result. The provider
// composes error cases such as unknown skills and unreadable bodies, so the
// executor only validates the arguments.
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
	parent, err := e.forkSkillParentRun()
	if err != nil {
		outcome.Content = "Error: could not derive fork skill context"
		return outcome
	}
	// SkillProvider decides whether the named skill is fork-mode. Inline skills
	// ignore this optional context and retain their existing activation path.
	ctx = agent.WithForkSkillParentRun(ctx, parent)
	body, err := e.deps.SkillProvider.LoadSkill(ctx, e.request.Work.SessionID, name, skillArgs)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "# Skill: "+name+"\n\n"+body
	return outcome
}

// forkSkillParentRun derives the fork child's authority and runtime inputs
// from the currently active parent run. The child factory enforces the same
// read-only tool allowlist used by delegate_tasks.
func (e *toolRunExecutor) forkSkillParentRun() (agent.ParentRun, error) {
	permissionBounds, err := json.Marshal(e.authority)
	if err != nil {
		return agent.ParentRun{}, err
	}
	childDeps := e.deps
	childDeps.SessionRoot = "" // child tool calls are not parent transcript events
	childDeps.HookRunner = nil
	childFactory := NewToolExecutorFactory(childDeps, WithReadOnlyTools())
	return agent.ParentRun{
		RunID: e.request.RunID, Deadline: e.request.RunDeadline, Work: e.request.Work,
		ProjectRoot: e.authority.AllowedRoot, PermissionBounds: permissionBounds,
		Provider: e.deps.Provider, ProviderName: e.request.ProviderName, Model: e.request.Model,
		ToolSchemas: ReadOnlyToolSchemas(), ExecutorFactory: childFactory,
	}, nil
}

func (e *toolRunExecutor) hookParentRun() agent.ParentRun {
	parent, err := e.forkSkillParentRun()
	if err == nil {
		return parent
	}
	// Keep enough event identity for non-agent hooks to proceed even when the
	// read-only child runtime is unavailable. An agent action will then fail
	// through its normal hook error policy.
	return agent.ParentRun{RunID: e.request.RunID, Deadline: e.request.RunDeadline, Work: e.request.Work}
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

// ---------------------------------------------------------------------------
// M07-C host-side MCP tool entries
//
// Direct mcp__<server>__<tool> calls, the mcp_call bridge and the read-only
// tool_search index join the M06 host branches: they never touch the
// filesystem or the sandbox helper, and they run behind the same pre-tool-use
// hook and EventToolCall/EventToolResult pairing as every other tool. The MCP
// dependency is optional — with no caller injected every entry falls through
// to mapTool's unknown-tool error, keeping existing deployments unchanged.
// ---------------------------------------------------------------------------

// maxToolSearchResults caps one tool_search listing regardless of the
// requested limit, so a broad query cannot flood the model context.
const maxToolSearchResults = 20

// toolSearchSchemaLimit bounds the rendered input_schema of one tool_search
// entry: dispatch-tier schemas only need to be recognizable, not complete.
const toolSearchSchemaLimit = 400

// executeMCPDirectCall serves model-emitted mcp__<server>__<tool> calls. The
// name is resolved through MCPCaller.ResolveTarget rather than a local split:
// ResolveTarget is the single authority on target names (full name,
// "server__tool" pair, unique bare suffix) and its failures already carry the
// available-tool guidance, so this entry and the mcp_call bridge cannot
// disagree about what a given name means.
func (e *toolRunExecutor) executeMCPDirectCall(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	server, tool, err := e.deps.MCP.ResolveTarget(call.Name)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	return e.executeMCPTarget(ctx, call, server, tool, args, outcome)
}

// executeMCPCall serves the mcp_call bridge: an explicit server/tool pair with
// optional arguments, resolved against the full dispatch inventory. The gate
// sees the resolved target, never the raw spelling, so approvals and saved
// rules always bind to the normalized server__tool pair.
func (e *toolRunExecutor) executeMCPCall(ctx context.Context, call llm.ToolUse, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	server, _ := args["server"].(string)
	tool, _ := args["tool"].(string)
	server, tool = strings.TrimSpace(server), strings.TrimSpace(tool)
	var query string
	switch {
	case server != "" && tool != "":
		query = server + "__" + tool
	case server != "":
		// The model stuffed the whole target into one field; let the
		// resolver make sense of it instead of guessing the split.
		query = server
	case tool != "":
		query = tool
	default:
		outcome.Content = "Error: server and tool are required"
		return outcome
	}
	resolvedServer, resolvedTool, err := e.deps.MCP.ResolveTarget(query)
	if err != nil {
		outcome.Content = "Error: " + err.Error()
		return outcome
	}
	callArgs := map[string]any{}
	if raw, present := args["arguments"]; present && raw != nil {
		object, isObject := raw.(map[string]any)
		if !isObject {
			outcome.Content = "Error: arguments must be an object"
			return outcome
		}
		callArgs = object
	}
	return e.executeMCPTarget(ctx, call, resolvedServer, resolvedTool, callArgs, outcome)
}

// executeMCPTarget runs one resolved MCP invocation: arguments are coerced
// against the target input schema when the caller can supply it, the
// permission gate arbitrates (ask flows through the same approval wait as the
// sandbox path), and the result keeps the server's own error flag instead of
// being prefixed locally.
func (e *toolRunExecutor) executeMCPTarget(ctx context.Context, call llm.ToolUse, server, tool string, args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	if schema, ok := e.deps.MCP.InputSchema(server, tool); ok {
		args = coerceMCPArguments(args, schema)
	}
	if e.deps.Gate == nil {
		outcome.Content = "Error: permission gate unavailable"
		return outcome
	}
	operation := permission.Operation{ID: call.ID, Kind: permission.OpMCPTool, Name: server, Target: server + "__" + tool}
	decision, err := e.deps.Gate.Authorize(ctx, e.authority, operation)
	if err != nil {
		outcome.Content = "Error: permission authorization failed"
		return outcome
	}
	if decision.Kind == permission.DecisionAsk {
		if e.approvalObserver != nil {
			e.approvalObserver()
		}
		decision, err = e.waitForApproval(ctx, operation)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Unlike the sandbox path this cannot abort the run from a
				// host branch, and finish() below still pairs the logged
				// tool_call event, so abortLoggedCall must not run here.
				outcome.Content = "Error: run cancelled while awaiting approval"
				return outcome
			}
			outcome.Content = "Error: approval unavailable"
			return outcome
		}
	}
	if decision.Kind != permission.DecisionAllow {
		outcome.Status = agent.ToolDenied
		outcome.Content = "Error: " + safeReason(decision.Reason, "operation denied")
		return outcome
	}
	output, isError, callErr := e.deps.MCP.CallTool(ctx, server, tool, args)
	if callErr != nil {
		// Transport failures are host-side conditions; their text is safe and
		// useful to surface verbatim.
		outcome.Content = "Error: mcp tool call failed: " + callErr.Error()
		return outcome
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, output
	if isError {
		outcome.Status = agent.ToolFailed
		outcome.IsError = true
	}
	return outcome
}

// executeToolSearch serves the read-only dispatch-tier index. It never calls
// a tool and constructs no permission operation, like ask_user and the task
// tools. Keywords are matched case-insensitively against name and
// description; an empty query lists everything, capped at
// maxToolSearchResults with the remainder noted.
func (e *toolRunExecutor) executeToolSearch(args map[string]any, outcome agent.ToolOutcome) agent.ToolOutcome {
	query, _ := args["query"].(string)
	query = strings.ToLower(strings.TrimSpace(query))
	terms := strings.Fields(query)
	limit := maxToolSearchResults
	if raw, ok := args["limit"].(float64); ok && raw >= 1 {
		limit = min(int(raw), maxToolSearchResults)
	}
	var matches []MCPToolSchema
	for _, tool := range e.deps.MCP.DispatchTools() {
		if matchesToolQuery(tool, terms) {
			matches = append(matches, tool)
		}
	}
	if len(matches) == 0 {
		if query == "" {
			outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, "No dispatch tools are available on the connected MCP servers."
			return outcome
		}
		outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, fmt.Sprintf("No dispatch tools match %q. Try broader keywords, or search with an empty query to list everything.", query)
		return outcome
	}
	shown := matches
	if len(shown) > limit {
		shown = shown[:limit]
	}
	var b strings.Builder
	if query == "" {
		fmt.Fprintf(&b, "Found %d dispatch tools", len(matches))
	} else {
		fmt.Fprintf(&b, "Found %d dispatch tools matching %q", len(matches), query)
	}
	if len(shown) < len(matches) {
		fmt.Fprintf(&b, " (showing %d, %d more not listed — refine the query):", len(shown), len(matches)-len(shown))
	} else {
		b.WriteString(":")
	}
	for _, tool := range shown {
		description := tool.Description
		if description == "" {
			description = "(no description)"
		}
		fmt.Fprintf(&b, "\n- %s: %s\n  schema: %s", tool.Name, description, truncateSchemaText(tool.InputSchema))
	}
	outcome.Status, outcome.IsError, outcome.Content = agent.ToolSucceeded, false, b.String()
	return outcome
}

// matchesToolQuery reports whether every whitespace-separated keyword appears
// in the tool's name or description (case-insensitive). An empty term list
// matches everything.
func matchesToolQuery(tool MCPToolSchema, terms []string) bool {
	haystack := strings.ToLower(tool.Name + "\n" + tool.Description)
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// truncateSchemaText renders one input schema for the tool_search listing,
// cut on a rune boundary when it would dominate the result.
func truncateSchemaText(schema map[string]any) string {
	if len(schema) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return "{}"
	}
	text := string(encoded)
	if runes := []rune(text); len(runes) > toolSearchSchemaLimit {
		return string(runes[:toolSearchSchemaLimit]) + "...[truncated]"
	}
	return text
}

// MCP argument coercion mirrors internal/mcp.CoerceBySchema shape for shape so
// the executor bridge and the SDK-side dispatch cannot produce different
// arguments for the same call. internal/mcp imports the MCP SDK and this
// package must not, so the rules are duplicated here on purpose; change
// internal/mcp/coerce.go first, then re-derive this twin, keeping the
// cross-language rule table intact.
var (
	mcpIntShape = regexp.MustCompile(`^[+-]?\d+$`)
	mcpNumShape = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
)

// coerceMCPArguments nudges model-written arguments toward the target input
// schema (numbers for declared strings, numeric strings for declared numbers,
// "true"/"false" for booleans, plus object/array recursion). Values that
// cannot be corrected pass through untouched: the MCP server's own domain
// error is more useful to the model than a local type error.
func coerceMCPArguments(args map[string]any, schema map[string]any) map[string]any {
	if args == nil {
		return args
	}
	if fixed, ok := mcpCoerceValue(args, schema).(map[string]any); ok {
		return fixed
	}
	return args
}

// mcpCoerceValue is the recursion core of coerceMCPArguments; the any signature
// handles both nested schemas (properties, items) and nested values.
func mcpCoerceValue(value any, schema any) any {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		return value
	}
	want, _ := schemaMap["type"].(string)
	if want == "object" {
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}
		properties, _ := schemaMap["properties"].(map[string]any)
		out := make(map[string]any, len(object))
		for key, item := range object {
			if sub, found := properties[key]; found {
				out[key] = mcpCoerceValue(item, sub)
			} else {
				out[key] = item
			}
		}
		return out
	}
	if want == "array" {
		itemSchema := schemaMap["items"]
		// Models often wrap arrays in a single-key object or join them into
		// one comma-separated string.
		if object, isObj := value.(map[string]any); isObj && len(object) == 1 {
			for _, inner := range object {
				if arr, isArr := inner.([]any); isArr {
					value = arr
				}
			}
		} else if text, isStr := value.(string); isStr {
			parts := strings.Split(text, ",")
			arr := make([]any, 0, len(parts))
			for _, part := range parts {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					arr = append(arr, trimmed)
				}
			}
			value = arr
		}
		if arr, isArr := value.([]any); isArr {
			out := make([]any, len(arr))
			for i, item := range arr {
				out[i] = mcpCoerceValue(item, itemSchema)
			}
			return out
		}
		return value
	}
	if want != "" {
		return mcpCoerceScalar(value, want)
	}
	return value
}

func mcpCoerceScalar(value any, want string) any {
	switch want {
	case "string":
		// A bool must not silently become the text "true".
		switch v := value.(type) {
		case float64:
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case json.Number:
			return v.String()
		}
	case "integer":
		if text, ok := value.(string); ok {
			trimmed := strings.TrimSpace(text)
			// "5.7" against integer is not truncated here; the MCP server
			// reports its own domain error.
			if mcpIntShape.MatchString(trimmed) {
				if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
					return n
				}
			}
		}
	case "number":
		if text, ok := value.(string); ok {
			trimmed := strings.TrimSpace(text)
			if mcpNumShape.MatchString(trimmed) {
				if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
					return f
				}
			}
		}
	case "boolean":
		if text, ok := value.(string); ok {
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "true":
				return true
			case "false":
				return false
			}
		}
	}
	return value
}

var _ agent.ExecutorFactory = ToolExecutorFactory{}
