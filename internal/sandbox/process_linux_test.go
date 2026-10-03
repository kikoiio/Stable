//go:build linux

package sandbox

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRunProcessGroupStopsDescendantsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := runProcessGroup(ctx, cmd); err == nil {
		t.Fatal("process group completed successfully after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("process group cleanup took too long: %s", elapsed)
	}
}
