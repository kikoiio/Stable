package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/platform/sandbox"
	"stable/internal/store"
)

// snapshotWriteSandbox performs write_file helper calls against the host
// candidate root so checkpointing observes real content changes; command
// calls are simulated as no-ops that change nothing.
type snapshotWriteSandbox struct{}

func (s *snapshotWriteSandbox) Probe(context.Context, sandbox.SandboxProfile) error { return nil }
func (s *snapshotWriteSandbox) RunIsolated(_ context.Context, p sandbox.SandboxProfile, argv []string, stdin io.Reader) (sandbox.SandboxResult, error) {
	if len(argv) > 0 && argv[0] == "bash" {
		return sandbox.SandboxResult{Stdout: []byte("ok"), ExitCode: 0}, nil
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return sandbox.SandboxResult{}, err
	}
	var request HelperRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		return sandbox.SandboxResult{}, err
	}
	if request.Tool != "write_file" {
		out, _ := json.Marshal(HelperResponse{Output: "read"})
		return sandbox.SandboxResult{Stdout: out}, nil
	}
	rel, _ := request.Args["file_path"].(string)
	rel = strings.TrimPrefix(rel, "/workspace/candidate/")
	content, _ := request.Args["content"].(string)
	target := filepath.Join(p.CandidateRoot, rel)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return sandbox.SandboxResult{}, err
	}
	if err = os.WriteFile(target, []byte(content), 0600); err != nil {
		return sandbox.SandboxResult{}, err
	}
	out, _ := json.Marshal(HelperResponse{Output: "wrote " + rel, Additions: 1})
	return sandbox.SandboxResult{Stdout: out}, nil
}
func (s *snapshotWriteSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("unused")
}
func (s *snapshotWriteSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("unused")
}
func (s *snapshotWriteSandbox) StopIsolatedSession(context.Context, string) error { return nil }

// snapshotLifecycle records candidate transitions so tests can observe the
// persisted block.
type snapshotLifecycle struct{ status map[string]string }

func (l *snapshotLifecycle) SaveCandidate(_ context.Context, r store.CandidateRecord) error {
	if l.status == nil {
		l.status = map[string]string{}
	}
	l.status[r.Candidate.ID] = "prepared"
	return nil
}
func (l *snapshotLifecycle) TransitionCandidate(_ context.Context, id, from, to, _ string) error {
	if l.status[id] != from {
		return fmt.Errorf("candidate %s is %q, not %q", id, l.status[id], from)
	}
	l.status[id] = to
	return nil
}

// failingSnapshots injects checkpoint failures: the first failAfter calls
// succeed, later calls fail.
type failingSnapshots struct {
	failAfter int
	calls     int
	labels    []string
}

func (f *failingSnapshots) Create(sessionID, candidateID, runID, label, _ string) (candidate.FileSnapshot, error) {
	f.calls++
	if f.calls > f.failAfter {
		return candidate.FileSnapshot{}, errors.New("snapshot quota exhausted")
	}
	f.labels = append(f.labels, label)
	return candidate.FileSnapshot{
		SnapshotID:  fmt.Sprintf("snap-%d", f.calls),
		SessionID:   sessionID,
		CandidateID: candidateID,
		RunID:       runID,
		Label:       label,
		Digest:      fmt.Sprintf("digest-%d", f.calls),
		CreatedAt:   time.Now().UTC(),
	}, nil
}

func snapshotExecutor(t *testing.T, formal, candidateRoot string, snapshots SnapshotCreator) agent.RunExecutor {
	t.Helper()
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate:       gate,
		Sandbox:    &snapshotWriteSandbox{},
		HelperPath: "helper",
		Now:        time.Now,
		Candidates: &snapshotLifecycle{},
		Snapshots:  snapshots,
	})
	executor, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func writeCall(id, path, content string) llm.ToolUse {
	args, _ := json.Marshal(map[string]string{"file_path": path, "content": content})
	return llm.ToolUse{ID: id, Name: "write_file", Arguments: args}
}

// A successful write checkpoints the pre-state and the post-state; a later
// write reuses the previous post-state as its pre-state, and a command that
// changes nothing creates no snapshots at all.
func TestToolExecutorSnapshotsWriteBoundaries(t *testing.T) {
	formal := t.TempDir()
	if err := os.WriteFile(filepath.Join(formal, "seed.txt"), []byte("seed"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	snapshots, err := candidate.NewSnapshotStore(formal, 1<<20, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := snapshotExecutor(t, formal, candidateRoot, snapshots)

	outcome, err := executor.Execute(context.Background(), writeCall("call-1", "notes.txt", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.IsError || len(outcome.Snapshots) != 2 {
		t.Fatalf("first write outcome = %+v", outcome)
	}
	if outcome.Snapshots[0].Label != "pre:write_file" || outcome.Snapshots[1].Label != "post:write_file" {
		t.Fatalf("snapshot labels = %+v", outcome.Snapshots)
	}
	for _, meta := range outcome.Snapshots {
		if meta.SnapshotID == "" || meta.CandidateID != "candidate" || meta.RunID != "run-1" || meta.Digest == "" || meta.CreatedAt.IsZero() {
			t.Fatalf("incomplete snapshot meta = %+v", meta)
		}
	}
	preDigest := outcome.Snapshots[0].Digest

	outcome, err = executor.Execute(context.Background(), writeCall("call-2", "notes.txt", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.IsError || len(outcome.Snapshots) != 1 || outcome.Snapshots[0].Label != "post:write_file" {
		t.Fatalf("second write should only checkpoint the post-state: %+v", outcome)
	}
	if outcome.Snapshots[0].Digest == preDigest {
		t.Fatal("post-state digest did not move after the write")
	}

	outcome, err = executor.Execute(context.Background(), llm.ToolUse{ID: "call-3", Name: "command", Arguments: json.RawMessage(`{"command":"ls"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.IsError || len(outcome.Snapshots) != 0 {
		t.Fatalf("no-change command must not checkpoint: %+v", outcome)
	}

	outcome, err = executor.Execute(context.Background(), llm.ToolUse{ID: "call-4", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"seed.txt"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Snapshots) != 0 {
		t.Fatalf("read-only call must not checkpoint: %+v", outcome)
	}

	listed, err := snapshots.List("candidate")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("stored manifests = %d, want 3", len(listed))
	}
	if _, err = snapshots.ValidateRestore("candidate", listed[0].SnapshotID); err != nil {
		t.Fatalf("pre-state snapshot does not restore: %v", err)
	}
}

// A failed pre-state snapshot blocks the candidate: the write never runs,
// later mutations are denied, and the persisted candidate leaves the running
// state so it can never be frozen ready and accepted.
func TestToolExecutorPreSnapshotFailureBlocksCandidate(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	lifecycle := &snapshotLifecycle{}
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate:       gate,
		Sandbox:    &snapshotWriteSandbox{},
		HelperPath: "helper",
		Now:        time.Now,
		Candidates: lifecycle,
		Snapshots:  &failingSnapshots{failAfter: 0},
	})
	executor, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.Execute(context.Background(), writeCall("call-1", "notes.txt", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolFailed || !outcome.IsError || !strings.Contains(outcome.Content, "pre-change snapshot failed") {
		t.Fatalf("pre-snapshot failure outcome = %+v", outcome)
	}
	if _, statErr := os.Lstat(filepath.Join(candidateRoot, "notes.txt")); !os.IsNotExist(statErr) {
		t.Fatal("write executed despite the failed pre-state snapshot")
	}
	if lifecycle.status["candidate"] != "blocked" {
		t.Fatalf("candidate status = %q, want blocked", lifecycle.status["candidate"])
	}

	outcome, err = executor.Execute(context.Background(), writeCall("call-2", "other.txt", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !strings.Contains(outcome.Content, "candidate is blocked") {
		t.Fatalf("blocked candidate accepted a write: %+v", outcome)
	}
}

// A failed post-state snapshot blocks the candidate too and the tool result
// reports the failure instead of faking a successful checkpoint.
func TestToolExecutorPostSnapshotFailureBlocksCandidate(t *testing.T) {
	formal := t.TempDir()
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	lifecycle := &snapshotLifecycle{}
	gate := &executorTestGate{decision: permission.PermissionDecision{Kind: permission.DecisionAllow}}
	factory := NewToolExecutorFactory(ToolExecutorDeps{
		Gate:       gate,
		Sandbox:    &snapshotWriteSandbox{},
		HelperPath: "helper",
		Now:        time.Now,
		Candidates: lifecycle,
		Snapshots:  &failingSnapshots{failAfter: 1},
	})
	executor, err := factory.ForRun(executorRequest(t, formal, candidateRoot))
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.Execute(context.Background(), writeCall("call-1", "notes.txt", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolFailed || !outcome.IsError || !strings.Contains(outcome.Content, "post-change snapshot failed") {
		t.Fatalf("post-snapshot failure was not observable: %+v", outcome)
	}
	if len(outcome.Snapshots) != 1 || outcome.Snapshots[0].Label != "pre:write_file" {
		t.Fatalf("pre-state checkpoint missing from outcome: %+v", outcome.Snapshots)
	}
	if lifecycle.status["candidate"] != "blocked" {
		t.Fatalf("candidate status = %q, want blocked", lifecycle.status["candidate"])
	}

	outcome, err = executor.Execute(context.Background(), writeCall("call-2", "other.txt", "two"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != agent.ToolDenied || !strings.Contains(outcome.Content, "candidate is blocked") {
		t.Fatalf("blocked candidate accepted a write: %+v", outcome)
	}
}
