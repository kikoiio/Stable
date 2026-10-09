//go:build linux

package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestMissingTrackedLeaderDoesNotSignalSurvivingProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & echo $! > \"$1\"; exit 0", "tracked", childPIDFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(cmd.Environ(), "STABLE_WORKSPACE_PROCESS_TOKEN="+strings.Repeat("a", 64))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	started, err := ProcessStartTime(pid)
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	childBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childBytes)))
	if err != nil {
		t.Fatalf("parse child PID: %v", err)
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	identity := TrackedProcess{PID: pid, ProcessGroup: pid, StartTimeTicks: started, Token: strings.Repeat("a", 64), WorkspaceID: "workspace", RunID: "run", Generation: 1}
	if err := StopTrackedProcess(identity, 10_000_000); !errors.Is(err, ErrProcessIdentity) {
		t.Fatalf("recovery accepted missing leader with live descendant: %v", err)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("recovery unexpectedly signaled descendant: %v", err)
	}
}
