package proc

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

var (
	ErrProcessIdentity           = errors.New("proc: tracked process identity does not match")
	ErrTrackedProcessUnavailable = errors.New("proc: tracked process termination is unavailable")
)

// TrackedProcess is a host child whose identity was durably recorded before
// its work was allowed to continue. Token is supplied in the child's initial
// environment and is checked again before any recovery signal is sent.
type TrackedProcess struct {
	PID            int    `json:"pid"`
	ProcessGroup   int    `json:"process_group"`
	StartTimeTicks uint64 `json:"start_time_ticks"`
	Token          string `json:"token"`
	WorkspaceID    string `json:"workspace_id"`
	RunID          string `json:"run_id"`
	Generation     uint64 `json:"generation"`
}

// Alive reports whether cmd's process is still running and not a zombie.
func Alive(cmd *exec.Cmd) bool {
	return alive(cmd)
}

// StopProcess sends SIGTERM, waits grace, then Kill.
func StopProcess(cmd *exec.Cmd, grace time.Duration) {
	stopProcess(cmd, grace)
}

// Terminate requests graceful termination of a process.
func Terminate(p *os.Process) error { return terminate(p) }

// AdoptChild applies the platform's parent-death/process-tree policy after a
// command has been started.
func AdoptChild(cmd *exec.Cmd) error { return adoptChild(cmd) }

// StopGroup signals the process group, then SIGKILL after the first grace.
func StopGroup(pgid int, termGrace, killGrace time.Duration) error {
	return stopGroup(pgid, termGrace, killGrace)
}

// KillGroup sends SIGTERM, or SIGKILL when force is true, to the process group.
func KillGroup(pgid int, force bool) error {
	return killGroup(pgid, force)
}

// ConfigureChild sets process-group and parent-death attributes on cmd.
func ConfigureChild(cmd *exec.Cmd) error {
	return configureChild(cmd)
}

// Cmdline reads a process command line where the platform exposes a safe
// query. Unsupported platforms return an explicit error.
func Cmdline(pid int) (string, error) {
	return cmdline(pid)
}

// ProcessStartTime returns the kernel start-time counter used to distinguish
// a live process from a later process that reused its PID.
func ProcessStartTime(pid int) (uint64, error) { return processStartTime(pid) }

// ProcessGroupActive reports whether a process group still has a live member.
// Zombies are excluded because they cannot mutate files or handle signals.
func ProcessGroupActive(group int) (bool, error) { return processGroupActive(group) }

// StopTrackedProcess verifies a durable process identity before terminating
// its process group. Unsupported platforms fail closed.
func StopTrackedProcess(process TrackedProcess, timeout time.Duration) error {
	return stopTrackedProcess(process, timeout)
}
