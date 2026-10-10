package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/llm"
	"stable/internal/workspace"
)

func TestWorkspaceWriterStaleExecutorCannotWriteIntoNextGeneration(t *testing.T) {
	manager, lease, executor, fake := writerExecutorFixture(t)
	formalFile := filepath.Join(lease.Scope.Authority.FormalRoot, "base.txt")
	formalBefore, err := os.ReadFile(formalFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReleaseCompletedWriter(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	const nextRunID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	nextScope := lease.Scope
	nextScope.Authority.RunID = "lead"
	nextScope.Authority.AllowedRoot = nextScope.Authority.FormalRoot
	nextScope.Authority.CandidateRoot = filepath.Join(filepath.Dir(lease.Paths.Root), "unused-candidate")
	nextLease, err := manager.AcquireWriter(context.Background(), nextScope, lease.WorkspaceID, nextRunID)
	if err != nil {
		t.Fatalf("acquire next writer generation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = manager.ReleaseCompletedWriter(context.Background(), nextLease)
	})
	if nextLease.Generation <= lease.Generation {
		t.Fatalf("generation did not advance: old=%d new=%d", lease.Generation, nextLease.Generation)
	}

	outcome, err := executor.Execute(context.Background(), llm.ToolUse{
		ID: "stale-write", Name: "write_file",
		Arguments: json.RawMessage(`{"file_path":"late-write.txt","content":"must not reach the next generation"}`),
	})
	if err != nil || !outcome.IsError {
		t.Fatalf("stale write outcome=%+v err=%v, want rejection", outcome, err)
	}
	if fake.profile.CandidateRoot != "" || len(fake.argv) != 0 {
		t.Fatalf("stale write reached sandbox: profile=%+v argv=%v", fake.profile, fake.argv)
	}
	current, err := manager.Get(context.Background(), lease.Scope, lease.WorkspaceID)
	if err != nil || current.State != workspace.StateWriting || current.WriterRunID != nextLease.RunID || current.Generation != nextLease.Generation {
		t.Fatalf("stale writer disturbed current lease: snapshot=%+v err=%v next=%+v", current, err, nextLease)
	}
	formalAfter, err := os.ReadFile(formalFile)
	if err != nil || string(formalAfter) != string(formalBefore) {
		t.Fatalf("formal bytes changed: before=%q after=%q err=%v", formalBefore, formalAfter, err)
	}
	if _, err := os.Stat(filepath.Join(nextLease.Paths.Checkout, "late-write.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale write reached next generation checkout: %v", err)
	}
	if current.ID != lease.WorkspaceID {
		t.Fatalf("workspace identity changed: got=%q want=%q", current.ID, lease.WorkspaceID)
	}
}
