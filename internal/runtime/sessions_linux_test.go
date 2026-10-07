//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"stable/internal/core"
	"stable/internal/platform/paths"
	"stable/internal/store"
)

func TestStopSessionProcessesIgnoresUnrelatedPID(t *testing.T) {
	state := t.TempDir()
	design := filepath.Join(state, "candidate.kicad_sch")
	if err := os.WriteFile(design, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	database := filepath.Join(state, "state.db")
	s, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateGoal(context.Background(), core.Goal{ID: "goal-1", Objective: "test", AllowedRoot: state}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	runtimeHandle, err := json.Marshal(sessionHandle{Path: design, Display: ":101", EeschemaPID: cmd.Process.Pid, XvfbPID: cmd.Process.Pid})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.UpsertSession(context.Background(), core.ComputerSession{ID: "computer-goal-1", GoalID: "goal-1", Status: "open", RuntimeHandle: string(runtimeHandle)}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	p := paths.Paths{State: state, Database: database}
	if stopped := StopSessionProcesses(p); stopped != 0 {
		t.Fatalf("StopSessionProcesses signalled %d unrelated process(es)", stopped)
	}
	if err = cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was stopped: %v", err)
	}
}
