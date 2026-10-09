//go:build linux

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func alive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil || cmd.Process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] != "Z"
}

func stopProcess(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
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
		return fmt.Errorf("proc: command is nil")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return nil
}

func cmdline(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "\x00", " "), nil
}

func processStat(pid int) (state byte, processGroup int, startTime uint64, err error) {
	if pid <= 0 {
		return 0, 0, 0, ErrProcessIdentity
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, err
	}
	// comm is parenthesized and can itself contain spaces or ')'. Fields after
	// its final ')' begin with stat field 3 (state).
	close := bytes.LastIndexByte(data, ')')
	if close < 0 || close+2 >= len(data) {
		return 0, 0, 0, ErrProcessIdentity
	}
	fields := strings.Fields(string(data[close+2:]))
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return 0, 0, 0, ErrProcessIdentity
	}
	group, groupErr := strconv.Atoi(fields[2])                 // field 5: pgrp
	started, startErr := strconv.ParseUint(fields[19], 10, 64) // field 22
	if groupErr != nil || startErr != nil {
		return 0, 0, 0, ErrProcessIdentity
	}
	return fields[0][0], group, started, nil
}

func processStartTime(pid int) (uint64, error) {
	_, _, started, err := processStat(pid)
	return started, err
}

func tokenMatches(pid int, token string) bool {
	if token == "" || strings.ContainsAny(token, "\x00\n=") {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	want := []byte("STABLE_WORKSPACE_PROCESS_TOKEN=" + token)
	for _, item := range bytes.Split(data, []byte{0}) {
		if bytes.Equal(item, want) {
			return true
		}
	}
	return false
}

func stopTrackedProcess(process TrackedProcess, timeout time.Duration) error {
	if process.PID <= 0 || process.ProcessGroup != process.PID || process.StartTimeTicks == 0 || process.Token == "" || process.WorkspaceID == "" || process.RunID == "" || process.Generation == 0 {
		return ErrProcessIdentity
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	state, group, started, err := processStat(process.PID)
	if errors.Is(err, os.ErrNotExist) {
		active, groupErr := processGroupActive(process.ProcessGroup)
		if groupErr != nil {
			return groupErr
		}
		if active {
			return ErrProcessIdentity // a descendant survived, but the leader token is gone
		}
		return nil
	}
	if err != nil {
		return err
	}
	if started != process.StartTimeTicks || group != process.ProcessGroup {
		return ErrProcessIdentity
	}
	// A pidfd pins the verified process so its PID cannot be reused while the
	// process group is being signaled and observed.
	pidfd, err := unix.PidfdOpen(process.PID, 0)
	if err != nil {
		return fmt.Errorf("open tracked process: %w", err)
	}
	defer unix.Close(pidfd)
	state, group, started, err = processStat(process.PID)
	if errors.Is(err, os.ErrNotExist) {
		active, groupErr := processGroupActive(process.ProcessGroup)
		if groupErr != nil {
			return groupErr
		}
		if active {
			return ErrProcessIdentity
		}
		return nil
	}
	if err != nil || started != process.StartTimeTicks || group != process.ProcessGroup {
		return ErrProcessIdentity
	}
	if state != 'Z' && !tokenMatches(process.PID, process.Token) {
		return ErrProcessIdentity
	}
	if err := syscall.Kill(-process.ProcessGroup, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGroupGone(process.ProcessGroup) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The pidfd is still held, so this numeric group ID cannot be reassigned to
	// a different leader while force termination is issued.
	if err := syscall.Kill(-process.ProcessGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline = time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGroupGone(process.ProcessGroup) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("proc: tracked process group %d did not exit", process.ProcessGroup)
}

func processGroupGone(group int) bool {
	active, err := processGroupActive(group)
	return err == nil && !active
}

func processGroupActive(group int) (bool, error) {
	if group <= 0 {
		return false, ErrProcessIdentity
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 0 {
			continue
		}
		state, processGroup, _, statErr := processStat(pid)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return false, statErr
		}
		if processGroup == group && state != 'Z' && state != 'X' {
			return true, nil
		}
	}
	return false, nil
}
