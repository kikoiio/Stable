package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/workspace"
)

func TestInstallMergedManifestKeepsIndependentFormalAndWorkspaceChanges(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	baseline := filepath.Join(root, "baseline")
	checkout := filepath.Join(root, "checkout")
	target := filepath.Join(root, "candidate")
	for _, path := range []string{formal, baseline, checkout, target} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, files := range map[string]map[string]string{
		baseline: {"formal.txt": "base", "workspace.txt": "base", "removed.txt": "base"},
		formal:   {"formal.txt": "current", "workspace.txt": "base"},
		checkout: {"formal.txt": "base", "workspace.txt": "worker"},
		target:   {"old.txt": "stale"},
	} {
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	limits := workspace.DefaultLimits()
	base, err := workspace.BuildManifest(context.Background(), baseline, limits)
	if err != nil {
		t.Fatal(err)
	}
	formalManifest, err := workspace.BuildManifest(context.Background(), formal, limits)
	if err != nil {
		t.Fatal(err)
	}
	workspaceManifest, err := workspace.BuildManifest(context.Background(), checkout, limits)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := workspace.ThreeWayPreview(base, formalManifest, workspaceManifest, limits)
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("merge preview=%+v, err=%v", preview, err)
	}
	if err := installMergedManifest(context.Background(), target, formal, checkout, formalManifest, workspaceManifest, preview.Manifest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"formal.txt": "current", "workspace.txt": "worker"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q, err=%v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "removed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("formal deletion was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale target content survived: %v", err)
	}
}

func TestCanSwitchWorkspaceRequiresIdleOwningSession(t *testing.T) {
	work := agent.WorkRef{Kind: agent.WorkSession, SessionID: "session-1"}
	svc := &Service{activeRuns: map[string]string{}, activeRequests: map[string]agent.ExecutionRequest{}}
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); err != nil {
		t.Fatalf("idle session cannot switch workspace: %v", err)
	}
	svc.activeRuns["run-1"] = work.SessionID
	svc.activeRequests["run-1"] = agent.ExecutionRequest{RunID: "run-1", Work: work}
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active session run allowed workspace switch: %v", err)
	}
	svc.activeRuns = map[string]string{}
	svc.activeRequests = map[string]agent.ExecutionRequest{}
	tasks := NewAgentTaskCoordinator()
	tasks.active["task-1"] = &agentTaskState{work: work}
	svc.deps.AgentTasks = tasks
	if err := svc.CanSwitchWorkspace(context.Background(), workspace.Scope{SessionID: work.SessionID, Work: work}); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active background task allowed workspace switch: %v", err)
	}
}
