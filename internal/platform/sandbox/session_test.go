package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
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

func TestPersistentSessionRejectsNetworkAuthorization(t *testing.T) {
	p := SandboxProfile{
		ProjectRoot:   filepath.Join(t.TempDir(), "project"),
		CandidateRoot: filepath.Join(t.TempDir(), "candidate"),
		RunRoot:       filepath.Join(t.TempDir(), "run"),
		CandidateID:   "candidate-1",
		SessionArgv:   []string{"/bin/true"},
		NetworkGrants: []permission.NetworkGrant{{Protocol: "tcp", Host: "target.test", Port: 443, ResolvedIPs: []string{"127.0.0.1"}}},
	}
	_, err := (LinuxManager{}).StartIsolatedSession(nil, p)
	if !errors.Is(err, ErrSessionNetworkUnsupported) || !errors.Is(err, ErrNetworkGrantInvalid) {
		t.Fatalf("persistent session grant was not classified: %v", err)
	}
}
