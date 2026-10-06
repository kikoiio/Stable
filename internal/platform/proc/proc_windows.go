//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var childJobs sync.Map // pid -> windows.Handle; keeping the handle open owns the tree

func alive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
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
	if job, ok := childJobs.LoadAndDelete(cmd.Process.Pid); ok {
		_ = windows.CloseHandle(job.(windows.Handle))
	}
}

func killGroup(int, bool) error {
	return errors.New("proc: process groups are unsupported on Windows; Job Object owns children")
}
func stopGroup(int, time.Duration, time.Duration) error {
	return errors.New("proc: process groups are unsupported on Windows; Job Object owns children")
}
func configureChild(*exec.Cmd) error { return nil }
func cmdline(int) (string, error) {
	return "", errors.New("proc: command line query is unsupported on Windows")
}
func terminate(p *os.Process) error {
	if p == nil {
		return errors.New("proc: process is nil")
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
func adoptChild(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("proc: command is not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	procHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(procHandle)
	if err = windows.AssignProcessToJobObject(job, procHandle); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	childJobs.Store(cmd.Process.Pid, job)
	return nil
}
