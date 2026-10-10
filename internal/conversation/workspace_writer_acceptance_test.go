//go:build linux

package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/proc"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

type writerAcceptanceGate struct{}

func (writerAcceptanceGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
}

type workspaceCrashMarker struct {
	WorkspaceID string              `json:"workspace_id"`
	RunID       string              `json:"run_id"`
	Generation  uint64              `json:"generation"`
	Checkout    string              `json:"checkout"`
	Process     proc.TrackedProcess `json:"process"`
}

// crashMarkerAccounting makes the durable process identity visible to the
// parent test only after LifecycleService has journaled it. This lets the
// parent kill the test helper at the exact point where recovery matters.
type crashMarkerAccounting struct {
	*workspace.LifecycleService
	markerPath string
	marker     workspaceCrashMarker
}

func (a crashMarkerAccounting) RegisterWriterProcess(ctx context.Context, lease workspace.WriterLease, process proc.TrackedProcess) error {
	if err := a.LifecycleService.RegisterWriterProcess(ctx, lease, process); err != nil {
		return err
	}
	a.marker.WorkspaceID = lease.WorkspaceID
	a.marker.RunID = lease.RunID
	a.marker.Generation = lease.Generation
	a.marker.Checkout = lease.Paths.Checkout
	a.marker.Process = process
	data, err := json.Marshal(a.marker)
	if err != nil {
		return err
	}
	tmp := a.markerPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, a.markerPath)
}

// TestWorkspaceWriterAbruptBwrapDeathRecoversJournalIdempotently runs only in
// the dedicated Linux cloud workflow. A child test process starts a real
// bwrap command, journals its host process identity, and blocks in sleep. The
// parent then SIGKILLs that helper (so no Go defer or service Close runs),
// reopens LifecycleService, and verifies safe settlement plus idempotent
// repeated recovery while the checkout remains preserved.
func TestWorkspaceWriterAbruptBwrapDeathRecoversJournalIdempotently(t *testing.T) {
	const helperEnv = "STABLE_M09_WRITER_RECOVERY_CHILD"
	if os.Getenv(helperEnv) == "1" {
		runWorkspaceWriterRecoveryChild(t)
		return
	}
	helper := os.Getenv("STABLE_M09_WRITER_HELPER")
	volume := os.Getenv("STABLE_M09_VOLUME")
	if helper == "" || volume == "" {
		t.Skip("requires cloud helper and disposable bounded disk volume")
	}
	if err := sandbox.BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}

	fixture, err := os.MkdirTemp(volume, "writer-crash-")
	if err != nil {
		t.Fatal(err)
	}
	marker := workspaceCrashMarker{}
	safeToRemove := false
	defer func() {
		if marker.Process.ProcessGroup <= 0 {
			safeToRemove = true
		} else if !safeToRemove {
			active, activeErr := proc.ProcessGroupActive(marker.Process.ProcessGroup)
			if activeErr == nil && active {
				// Only signal the exact durable identity published by the helper.
				// A failed recovery must not leave the long-running sandbox command
				// behind after the test reports its failure.
				if stopErr := proc.StopTrackedProcess(marker.Process, 5*time.Second); stopErr != nil {
					t.Errorf("stop remaining tracked writer before cleanup: %v", stopErr)
				}
				active, activeErr = proc.ProcessGroupActive(marker.Process.ProcessGroup)
			}
			safeToRemove = activeErr == nil && !active
		}
		if safeToRemove {
			if err := os.RemoveAll(fixture); err != nil {
				t.Errorf("remove test fixture after confirming writer process group is gone: %v", err)
			}
		} else {
			t.Errorf("preserving recovery fixture %s because writer process-group state is uncertain", fixture)
		}
	}()
	formal := filepath.Join(fixture, "formal")
	stateRoot := filepath.Join(fixture, "state")
	markerPath := filepath.Join(fixture, "tracked-process.json")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestWorkspaceWriterAbruptBwrapDeathRecoversJournalIdempotently$", "-test.v")
	child.Env = append(os.Environ(), helperEnv+"=1", "STABLE_M09_RECOVERY_FORMAL="+formal, "STABLE_M09_RECOVERY_STATE="+stateRoot, "STABLE_M09_RECOVERY_MARKER="+markerPath, "STABLE_M09_WRITER_HELPER="+helper)
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childExit := make(chan error, 1)
	go func() { childExit <- child.Wait() }()
	childWaited := false
	t.Cleanup(func() {
		if !childWaited && child.Process != nil {
			_ = child.Process.Kill()
			<-childExit
			childWaited = true
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(markerPath)
		if readErr == nil && json.Unmarshal(data, &marker) == nil && marker.Process.PID > 1 {
			if _, err := os.Stat(filepath.Join(marker.Checkout, "recovery-running.txt")); err == nil {
				break
			}
		}
		select {
		case childErr := <-childExit:
			childWaited = true
			t.Fatalf("recovery helper exited before the sandbox command became active: %v", childErr)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if marker.WorkspaceID == "" || marker.Generation == 0 || marker.Process.PID <= 1 {
		t.Fatalf("helper did not publish a journaled process identity: %+v", marker)
	}
	if _, err := os.Stat(filepath.Join(marker.Checkout, "recovery-running.txt")); err != nil {
		t.Fatalf("real bwrap command did not reach the workspace checkout: %v", err)
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("simulate abrupt test-helper death: %v", err)
	}
	select {
	case <-childExit:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL did not terminate the recovery helper")
	}
	childWaited = true

	scope := workspaceCrashScope(formal)
	reopen := func() *workspace.LifecycleService {
		t.Helper()
		layout, err := workspace.NewLayout(stateRoot, formal, "m09project")
		if err != nil {
			t.Fatal(err)
		}
		manager, err := workspace.NewService(layout, workspace.Limits{}, workspace.ServiceDependencies{})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	first := reopen()
	firstSnapshot, err := first.Get(context.Background(), scope, marker.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(marker.Checkout, "recovery-running.txt")); err != nil {
		t.Fatalf("recovery discarded the writer checkout: %v", err)
	}
	if firstSnapshot.State == workspace.StateKept {
		if firstSnapshot.WriterRunID != "" {
			t.Fatalf("settled workspace retained writer authority: %+v", firstSnapshot)
		}
		active, activeErr := proc.ProcessGroupActive(marker.Process.ProcessGroup)
		if activeErr != nil || active {
			t.Fatalf("workspace settled while its original process group may still be active: active=%v err=%v", active, activeErr)
		}
		safeToRemove = true
	} else if firstSnapshot.State != workspace.StateInterrupted && firstSnapshot.State != workspace.StateBlocked {
		t.Fatalf("recovery did not settle or conservatively block the writer: %+v", firstSnapshot)
	} else if firstSnapshot.WriterRunID != marker.RunID || firstSnapshot.Error == "" {
		t.Fatalf("blocked recovery lost the writer identity or reason: %+v", firstSnapshot)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := reopen()
	defer second.Close(context.Background())
	secondSnapshot, err := second.Get(context.Background(), scope, marker.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if secondSnapshot.State != firstSnapshot.State || secondSnapshot.Cursor != firstSnapshot.Cursor || secondSnapshot.WriterRunID != firstSnapshot.WriterRunID {
		t.Fatalf("repeated recovery was not idempotent: first=%+v second=%+v", firstSnapshot, secondSnapshot)
	}
	if _, err := os.Stat(filepath.Join(marker.Checkout, "recovery-running.txt")); err != nil {
		t.Fatalf("repeated recovery discarded the writer checkout: %v", err)
	}
	if original, err := os.ReadFile(filepath.Join(formal, "base.txt")); err != nil || string(original) != "formal baseline" {
		t.Fatalf("abrupt workspace writer changed the formal tree: %q err=%v", original, err)
	}
}

func runWorkspaceWriterRecoveryChild(t *testing.T) {
	formal := os.Getenv("STABLE_M09_RECOVERY_FORMAL")
	stateRoot := os.Getenv("STABLE_M09_RECOVERY_STATE")
	markerPath := os.Getenv("STABLE_M09_RECOVERY_MARKER")
	helper := os.Getenv("STABLE_M09_WRITER_HELPER")
	layout, err := workspace.NewLayout(stateRoot, formal, "m09project")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.Limits{}, workspace.ServiceDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "m09recovery0000000000000000000000"
	const leadRunID = "m09recoverylead000000000000000000"
	const childRunID = "m09recoverychild00000000000000000"
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}
	scope := workspace.Scope{ProjectID: "m09project", SessionID: sessionID, Work: work, Authority: permission.Authority{RunID: leadRunID, SessionID: sessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(formal, "candidate"), Mode: permission.ModeBypass}}
	created, err := manager.Create(context.Background(), scope, "abrupt writer recovery")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireWriter(context.Background(), scope, created.ID, childRunID)
	if err != nil {
		t.Fatal(err)
	}
	accounting := crashMarkerAccounting{LifecycleService: manager, markerPath: markerPath}
	base := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.New(), Gate: writerAcceptanceGate{}, HelperPath: helper})
	factory := execution.WorkspaceWriterExecutorFactory(base, lease, accounting)
	permissionBounds, err := json.Marshal(lease.Authority)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := factory.ForRun(agent.ExecutionRequest{RunID: childRunID, Work: work, PermissionBounds: permissionBounds})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), llm.ToolUse{ID: "long-running-command", Name: "command", Arguments: json.RawMessage(`{"command":"printf active > recovery-running.txt; exec sleep 120"}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("long-running sandbox command unexpectedly returned")
}

func workspaceCrashScope(formal string) workspace.Scope {
	const sessionID = "m09recovery0000000000000000000000"
	const leadRunID = "m09recoverylead000000000000000000"
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}
	return workspace.Scope{ProjectID: "m09project", SessionID: sessionID, Work: work, Authority: permission.Authority{RunID: leadRunID, SessionID: sessionID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: filepath.Join(formal, "candidate"), Mode: permission.ModeBypass}}
}

// This fixture is enabled by the dedicated cloud workflow. It executes the
// real agentworker helper and bwrap, with a fake model runner; a skipped local
// test is not evidence for the Linux isolation boundary.
func TestWorkspaceWriterRealSyncBackgroundDefinitionAndDirect(t *testing.T) {
	helper := os.Getenv("STABLE_M09_WRITER_HELPER")
	volume := os.Getenv("STABLE_M09_VOLUME")
	if helper == "" || volume == "" {
		t.Skip("requires cloud helper and disposable bounded disk volume")
	}
	if err := sandbox.BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"sync", "background", "definition", "direct"} {
		t.Run(entry, func(t *testing.T) {
			isolation := "none"
			if entry == "definition" {
				isolation = "worktree"
			}
			role := fmt.Sprintf("---\nname: builder\ndescription: bounded writer\nisolation: %s\n---\nWrite only the assigned checkout.\n", isolation)
			runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
				if input.WorkspaceID == "" || input.WorkspaceGeneration == 0 {
					return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "delegated child did not receive workspace ID and generation"}
				}
				executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, PermissionBounds: input.PermissionBounds})
				if err != nil {
					return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
				}
				calls := []llm.ToolUse{
					{ID: "read-base", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"base.txt"}`)},
					{ID: "write-base", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"base.txt","content":"isolated child bytes"}`)},
					{ID: "read-current", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"base.txt"}`)},
					{ID: "write-new", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"new.txt","content":"workspace only"}`)},
					{ID: "command", Name: "command", Arguments: json.RawMessage(`{"command":"printf shell-only > command.txt; test ! -s .git; if printf bad > .git 2>/dev/null; then exit 31; fi; if mkdir .stable/attack 2>/dev/null; then exit 32; fi; test ! -e /workspace/project/.stable; test ! -e /workspace/repo.git; test ! -e /workspace/state; test ! -e /workspace/candidate/../formal"}`)},
				}
				for _, call := range calls {
					out, err := executor.Execute(ctx, call)
					if err != nil || out.IsError {
						return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("%s: %s (%v)", call.ID, out.Content, err)}
					}
					if call.ID == "read-current" && !strings.Contains(out.Content, "isolated child bytes") {
						return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "read-after-write used baseline"}
					}
					if call.ID == "command" && !strings.Contains(out.Content, "Exit code 0") {
						return agent.ChildRunResult{Status: agent.DelegationFailed, Error: out.Content}
					}
				}
				for _, path := range []string{".git", ".stable/private", ".mewcode/config", "../formal/base.txt"} {
					args, _ := json.Marshal(map[string]any{"file_path": path, "content": "attack"})
					out, err := executor.Execute(ctx, llm.ToolUse{ID: "protected", Name: "write_file", Arguments: args})
					if err != nil || !out.IsError {
						return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "protected path was writable: " + path}
					}
				}
				return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "bounded workspace complete"}
			})
			svc, formal, session := newAgentTaskTestService(t, runner, role)
			state, err := os.MkdirTemp(volume, "writer-state-")
			if err != nil {
				t.Fatal(err)
			}
			svc.deps.WorkspaceStateRoot = state
			if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.New(), Gate: writerAcceptanceGate{}, HelperPath: helper})
			id, _ := sessionlog.NewID()
			parent := agentTaskTestParent(t, svc, session, id)
			parent.ExecutorFactory = factory
			parent.ToolSchemas = []llm.ToolSchema{{Name: "read_file"}, {Name: "glob"}, {Name: "grep"}, {Name: "write_file"}, {Name: "edit_file"}, {Name: "command"}}
			svc.mu.Lock()
			svc.activeRuns[parent.RunID] = session
			svc.activeRequests[parent.RunID] = agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
			svc.mu.Unlock()
			req := agent.AgentTaskRequest{AgentName: "builder", Instruction: "write bounded data", Isolation: "worktree", Background: entry != "sync"}
			if entry == "definition" {
				req.Isolation = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result, err := svc.deps.AgentTasks.run(ctx, parent, req, entry == "direct")
			if err != nil {
				t.Fatal(err)
			}
			if !result.Status.IsTerminal() {
				result, err = svc.deps.AgentTasks.Output(ctx, parent, result.ID, 30*time.Second)
			}
			if err != nil || result.Status != agent.DelegationSucceeded || result.WorkspaceID == "" || result.WorkspaceGeneration == 0 {
				t.Fatalf("writer result=%+v err=%v", result, err)
			}
			data, err := os.ReadFile(filepath.Join(formal, "base.txt"))
			if err != nil || string(data) != "formal bytes" {
				t.Fatalf("formal changed: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(formal, "new.txt")); !os.IsNotExist(err) {
				t.Fatalf("child wrote formal new file: %v", err)
			}
			root, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := svc.workspaceService(root)
			if err != nil {
				t.Fatal(err)
			}
			kept, err := manager.Get(ctx, scope, result.WorkspaceID)
			if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 3 {
				t.Fatalf("workspace not retained: %+v %v", kept, err)
			}
			svc.mu.Lock()
			delete(svc.activeRuns, parent.RunID)
			delete(svc.activeRequests, parent.RunID)
			svc.mu.Unlock()
		})
	}
}

func TestParallelWorkspaceWriterAgentsUseIndependentLeases(t *testing.T) {
	helper := os.Getenv("STABLE_M09_WRITER_HELPER")
	volume := os.Getenv("STABLE_M09_VOLUME")
	if helper == "" || volume == "" {
		t.Skip("requires cloud helper and disposable bounded disk volume")
	}
	if err := sandbox.BoundedWorkspaceVolume(volume); err != nil {
		t.Fatal(err)
	}
	started := make(chan agentTaskTestInvocation, 2)
	release := make(chan struct{}, 2)
	runner := agentTaskTestRunner(func(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
		invocation := agentTaskTestInvocation{ctx: ctx, input: input, release: make(chan struct{})}
		select {
		case started <- invocation:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
		select {
		case <-release:
		case <-ctx.Done():
			return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
		}
		executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, PermissionBounds: input.PermissionBounds})
		if err != nil {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
		}
		marker := "one"
		if strings.Contains(input.Task.Instruction, "writer two") {
			marker = "two"
		}
		args, _ := json.Marshal(map[string]string{"file_path": "parallel.txt", "content": marker})
		out, err := executor.Execute(ctx, llm.ToolUse{ID: "write-" + marker, Name: "write_file", Arguments: args})
		if err != nil || out.IsError {
			return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("write %s: %s (%v)", marker, out.Content, err)}
		}
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "writer " + marker + " complete"}
	})
	role := "---\nname: builder\ndescription: isolated writer\nisolation: worktree\n---\nWrite only the assigned checkout.\n"
	svc, formal, session := newAgentTaskTestService(t, runner, role)
	state, err := os.MkdirTemp(volume, "parallel-writer-state-")
	if err != nil {
		t.Fatal(err)
	}
	svc.deps.WorkspaceStateRoot = state
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	factory := execution.NewToolExecutorFactory(execution.ToolExecutorDeps{Sandbox: sandbox.New(), Gate: writerAcceptanceGate{}, HelperPath: helper})
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parent := agentTaskTestParent(t, svc, session, parentRunID)
	parent.ExecutorFactory = factory
	parent.ToolSchemas = []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}}
	svc.mu.Lock()
	svc.activeRuns[parent.RunID] = session
	svc.activeRequests[parent.RunID] = agent.ExecutionRequest{RunID: parent.RunID, Work: parent.Work, PermissionBounds: parent.PermissionBounds}
	svc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, err := svc.deps.AgentTasks.run(ctx, parent, agent.AgentTaskRequest{AgentName: "builder", Instruction: "writer one", Isolation: "worktree", Background: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.deps.AgentTasks.run(ctx, parent, agent.AgentTaskRequest{AgentName: "builder", Instruction: "writer two", Isolation: "worktree", Background: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkspaceID == "" || second.WorkspaceID == "" || first.WorkspaceID == second.WorkspaceID || first.WorkspaceGeneration == 0 || second.WorkspaceGeneration == 0 {
		t.Fatalf("parallel tasks did not receive independent workspace leases: first=%+v second=%+v", first, second)
	}
	invocationA := receiveAgentTaskInvocation(t, started)
	invocationB := receiveAgentTaskInvocation(t, started)
	if invocationA.input.ChildRunID == invocationB.input.ChildRunID || invocationA.ctx.Err() != nil || invocationB.ctx.Err() != nil {
		t.Fatalf("parallel writer runs are not independently active: %q / %q", invocationA.input.ChildRunID, invocationB.input.ChildRunID)
	}
	childIDs := map[string]bool{invocationA.input.ChildRunID: true, invocationB.input.ChildRunID: true}
	root, scope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := svc.workspaceService(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []agent.AgentTaskSnapshot{first, second} {
		active, err := manager.Get(ctx, scope, task.WorkspaceID)
		if err != nil || active.State != workspace.StateWriting || !childIDs[active.WriterRunID] || active.Generation != task.WorkspaceGeneration {
			t.Fatalf("independent writer lease is not active: snapshot=%+v err=%v task=%+v", active, err, task)
		}
	}
	release <- struct{}{}
	release <- struct{}{}
	for _, task := range []agent.AgentTaskSnapshot{first, second} {
		completed, err := svc.deps.AgentTasks.Output(ctx, parent, task.ID, 30*time.Second)
		if err != nil || completed.Status != agent.DelegationSucceeded {
			t.Fatalf("parallel writer task did not complete: snapshot=%+v err=%v", completed, err)
		}
		kept, err := manager.Get(ctx, scope, task.WorkspaceID)
		if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" || kept.ChangedFiles != 1 {
			t.Fatalf("parallel workspace was not settled independently: %+v err=%v", kept, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(formal, "base.txt")); err != nil || string(data) != "formal bytes" {
		t.Fatalf("formal baseline changed: %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(formal, "parallel.txt")); !os.IsNotExist(err) {
		t.Fatalf("parallel child wrote into formal project: %v", err)
	}
	svc.mu.Lock()
	delete(svc.activeRuns, parent.RunID)
	delete(svc.activeRequests, parent.RunID)
	svc.mu.Unlock()
}
