//go:build linux

package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/sessionlog"
	"stable/internal/workspace"
)

type writerAcceptanceGate struct{}

func (writerAcceptanceGate) Authorize(context.Context, permission.Authority, permission.Operation) (permission.PermissionDecision, error) {
	return permission.PermissionDecision{Kind: permission.DecisionAllow}, nil
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
