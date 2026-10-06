//go:build darwin

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func alive(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.Process != nil && cmd.Process.Signal(syscall.Signal(0)) == nil
}

func stopProcess(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = terminate(cmd.Process)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done
	}
}

func killGroup(pgid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pgid, sig)
}

func stopGroup(pgid int, termGrace, killGrace time.Duration) error {
	_ = killGroup(pgid, false)
	time.Sleep(termGrace)
	_ = killGroup(pgid, true)
	if killGrace > 0 {
		time.Sleep(killGrace)
	}
	return nil
}

func configureChild(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("proc: command is nil")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func cmdline(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("proc: invalid pid")
	}
	return "", errors.New("proc: Darwin command-line query is unavailable in the portable build")
}
