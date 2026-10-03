package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionGeneration(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runRoot, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := nextSessionGeneration(runRoot, "session-1", "candidate-1")
	if err != nil || first != 1 {
		t.Fatalf("first generation=%d err=%v", first, err)
	}
	second, err := nextSessionGeneration(runRoot, "session-1", "candidate-1")
	if err != nil || second != 2 {
		t.Fatalf("second generation=%d err=%v", second, err)
	}
	third, err := nextSessionGeneration(runRoot, "session-1", "candidate-2")
	if err != nil || third != 3 {
		t.Fatalf("candidate rebinding generation=%d err=%v", third, err)
	}
	if err = ValidateSession(SandboxSession{ID: "session-1", Generation: first, CandidateID: "candidate-1"}); err == nil {
		t.Fatal("old session handle was not invalidated")
	}
}
