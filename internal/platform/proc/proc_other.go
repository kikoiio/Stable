//go:build !linux

package proc

import (
	"errors"
	"os/exec"
	"time"
)

func alive(*exec.Cmd) bool { return false }

func stopProcess(*exec.Cmd, time.Duration) {}

func killGroup(int, bool) error {
	return errors.New("proc: process groups are only supported on Linux")
}

func stopGroup(int, time.Duration, time.Duration) error {
	return errors.New("proc: process groups are only supported on Linux")
}

func configureChild(*exec.Cmd) error {
	return errors.New("proc: child process attributes are only supported on Linux")
}

func cmdline(int) (string, error) {
	return "", errors.New("proc: /proc cmdline is only supported on Linux")
}
