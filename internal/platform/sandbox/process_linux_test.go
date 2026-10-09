//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"stable/internal/platform/proc"
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

func TestTrackedProcessGatePersistsIdentityBeforeCommandRuns(t *testing.T) {
	for _, failPersistence := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistence_error_%v", failPersistence), func(t *testing.T) {
			marker := t.TempDir() + "/ran"
			cmd := exec.Command("/bin/sh", "-c", "printf ran > \"$1\"", "command", marker)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			identity := proc.TrackedProcess{Token: strings.Repeat("a", 64), WorkspaceID: "workspace", RunID: "run", Generation: 1}
			start := func(proc.TrackedProcess) error {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("command ran before identity persistence: %v", err)
				}
				if failPersistence {
					return errors.New("journal unavailable")
				}
				return nil
			}
			exit := func(proc.TrackedProcess) error { return nil }
			err := runTrackedProcessGroup(context.Background(), cmd, identity, start, exit)
			if failPersistence {
				if err == nil {
					t.Fatal("command succeeded despite failed identity persistence")
				}
				if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("command ran after failed identity persistence: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("tracked command: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("command did not run after identity persistence: %v", err)
			}
		})
	}
}

func TestTrackedProcessExitStopsLingeringProcessGroup(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	identity := proc.TrackedProcess{Token: strings.Repeat("b", 64), WorkspaceID: "workspace", RunID: "run", Generation: 1}
	exited := false
	err := runTrackedProcessGroup(context.Background(), cmd, identity, func(proc.TrackedProcess) error { return nil }, func(process proc.TrackedProcess) error {
		active, activeErr := proc.ProcessGroupActive(process.ProcessGroup)
		if activeErr != nil {
			return activeErr
		}
		if active {
			return errors.New("process group remained active after command exit")
		}
		exited = true
		return nil
	})
	if err != nil {
		t.Fatalf("tracked command with child process: %v", err)
	}
	if !exited {
		t.Fatal("durable process identity was not cleared after group exit")
	}
}
