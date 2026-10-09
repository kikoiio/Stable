package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/llm"
)

func TestWorkspaceWriterRejectsSymlinkEscapeBeforeSandbox(t *testing.T) {
	_, lease, executor, fake := writerExecutorFixture(t)
	formalRoot := lease.Scope.Authority.FormalRoot
	formalSentinel := filepath.Join(formalRoot, "base.txt")
	formalBefore, err := os.ReadFile(formalSentinel)
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(lease.Paths.Checkout, "formal-escape")
	if err := os.Symlink(formalRoot, link); err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.Execute(context.Background(), llm.ToolUse{
		ID: "symlink-escape", Name: "write_file",
		Arguments: json.RawMessage(`{"file_path":"formal-escape/escaped.txt","content":"must be denied"}`),
	})
	if err != nil || !outcome.IsError {
		t.Fatalf("symlink escape outcome=%+v err=%v, want denied tool result", outcome, err)
	}
	if len(fake.argv) != 0 {
		t.Fatalf("symlink escape reached sandbox: argv=%v", fake.argv)
	}
	if _, err := os.Lstat(filepath.Join(formalRoot, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink escape created formal file: %v", err)
	}
	formalAfter, err := os.ReadFile(formalSentinel)
	if err != nil || string(formalAfter) != string(formalBefore) {
		t.Fatalf("formal sentinel changed: before=%q after=%q err=%v", formalBefore, formalAfter, err)
	}
}
