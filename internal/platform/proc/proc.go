package proc

import (
	"os"
	"os/exec"
	"time"
)

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
