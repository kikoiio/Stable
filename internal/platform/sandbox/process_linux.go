//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"stable/internal/platform/proc"
	"stable/internal/platform/secfile"
)

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.limit <= 0 {
		b.limit = 1 << 20
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return original, nil
}

func (m LinuxManager) RunIsolated(ctx context.Context, p SandboxProfile, argv []string, stdin io.Reader) (SandboxResult, error) {
	if err := m.Probe(ctx, p); err != nil {
		return SandboxResult{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return m.run(ctx, p, argv, stdin)
}

func (m LinuxManager) run(ctx context.Context, p SandboxProfile, argv []string, stdin io.Reader) (result SandboxResult, retErr error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var proxyCancel context.CancelFunc
	var proxyDone chan error
	var proxyDir string
	defer func() {
		var cleanupErr error
		if proxyCancel != nil {
			proxyCancel()
			if proxyDone != nil {
				select {
				case proxyErr := <-proxyDone:
					if proxyErr != nil && !errors.Is(proxyErr, context.Canceled) {
						cleanupErr = proxyErr
					}
				case <-time.After(2 * time.Second):
					cleanupErr = errors.New("network proxy did not stop")
				}
			}
		}
		if proxyDir != "" {
			if err := os.RemoveAll(proxyDir); err != nil && !os.IsNotExist(err) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("%w: %v", ErrCleanupFailed, cleanupErr))
		}
	}()
	if len(p.NetworkGrants) > 0 {
		if p.ProxyHelperPath == "" {
			return SandboxResult{}, networkGrantMessage("network proxy helper is unavailable")
		}
		runRoot, err := filepath.Abs(p.RunRoot)
		if err != nil {
			return SandboxResult{}, profileError(err)
		}
		proxyDir, err = os.MkdirTemp(filepath.Dir(runRoot), ".stable-network-")
		if err != nil {
			return SandboxResult{}, fmt.Errorf("create private proxy directory: %w", err)
		}
		if err = secfile.ChmodPrivate(proxyDir, 0700); err != nil {
			return SandboxResult{}, err
		}
		socketPath := filepath.Join(proxyDir, "proxy.sock")
		guestSocket := "/run/stable-network/proxy.sock"
		proxyCtx, stopProxy := context.WithCancel(runCtx)
		proxyCancel = stopProxy
		proxyDone = make(chan error, 1)
		go func() {
			proxyDone <- (NetworkProxy{SocketPath: socketPath, Grants: p.NetworkGrants, Resolver: net.DefaultResolver}).Serve(proxyCtx)
		}()
		if err = waitForProxySocket(runCtx, socketPath, proxyDone); err != nil {
			return SandboxResult{}, fmt.Errorf("%w: start network proxy: %w", ErrUnavailable, err)
		}
		p.ReadOnlyMounts = append(p.ReadOnlyMounts, ReadOnlyMount{HostPath: proxyDir, GuestPath: "/run/stable-network"})
		p.ReadOnlyFiles = append(p.ReadOnlyFiles, ReadOnlyFileMount{HostPath: p.ProxyHelperPath, GuestPath: "/workspace/runtime/agentworker"})
		wrapped := []string{"/workspace/runtime/agentworker", "--stable-sandbox-proxy", guestSocket, "--"}
		argv = append(wrapped, argv...)
	}
	args, err := m.args(p, argv)
	if err != nil {
		return SandboxResult{}, err
	}
	cmd := m.command(runCtx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if p.WorkspaceProcess != nil {
		processEnv := cmd.Env
		if processEnv == nil {
			processEnv = os.Environ()
		}
		cmd.Env = withoutWorkspaceProcessToken(processEnv)
		cmd.Env = append(cmd.Env, "STABLE_WORKSPACE_PROCESS_TOKEN="+p.WorkspaceProcess.Token)
	}
	cmd.Stdin = stdin
	stdout := &limitedBuffer{limit: p.OutputLimit}
	stderr := &limitedBuffer{limit: p.OutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if p.WorkspaceProcess != nil {
		err = runTrackedProcessGroup(runCtx, cmd, *p.WorkspaceProcess, p.OnProcessStart, p.OnProcessExit)
	} else {
		err = runProcessGroup(runCtx, cmd)
	}
	result = SandboxResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return result, errors.Join(runCtx.Err(), err)
	}
	if runCtx.Err() == context.Canceled {
		return result, errors.Join(runCtx.Err(), err)
	}
	if err == nil {
		result.ExitCode = 0
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func runProcessGroup(ctx context.Context, cmd *exec.Cmd) error {
	return runProcessGroupWithIdentity(ctx, cmd, nil, nil, nil)
}

func runTrackedProcessGroup(ctx context.Context, cmd *exec.Cmd, identity proc.TrackedProcess, onStart, onExit func(proc.TrackedProcess) error) error {
	return runProcessGroupWithIdentity(ctx, cmd, &identity, onStart, onExit)
}

func runProcessGroupWithIdentity(ctx context.Context, cmd *exec.Cmd, identity *proc.TrackedProcess, onStart, onExit func(proc.TrackedProcess) error) error {
	var release func() error
	var closeGateReader func()
	var closeGate func()
	if identity != nil {
		if onStart == nil || onExit == nil {
			return errors.New("workspace sandbox process identity persistence is unavailable")
		}
		var err error
		release, closeGateReader, closeGate, err = gateTrackedCommand(cmd)
		if err != nil {
			return fmt.Errorf("prepare workspace sandbox process gate: %w", err)
		}
	}
	if err := cmd.Start(); err != nil {
		if closeGate != nil {
			closeGate()
		}
		return err
	}
	if closeGate != nil {
		closeGateReader()
	}
	pid := cmd.Process.Pid
	pidfd := -1
	if identity != nil {
		openedPIDFD, pinErr := unix.PidfdOpen(pid, 0)
		if pinErr != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			closeGate()
			_ = cmd.Wait()
			return fmt.Errorf("pin workspace sandbox process identity: %w", pinErr)
		}
		pidfd = openedPIDFD
		defer unix.Close(pidfd)
		started, err := proc.ProcessStartTime(pid)
		if err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			closeGate()
			_ = cmd.Wait()
			return fmt.Errorf("record workspace sandbox process identity: %w", err)
		}
		identity.PID, identity.ProcessGroup, identity.StartTimeTicks = pid, pid, started
		if err := onStart(*identity); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			closeGate()
			_ = cmd.Wait()
			return fmt.Errorf("persist workspace sandbox process identity: %w", err)
		}
		if err := release(); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			waitErr := cmd.Wait()
			cleanupErr := stopLingeringProcessGroup(pid, 2*time.Second)
			if cleanupErr == nil {
				cleanupErr = onExit(*identity)
			}
			return errors.Join(fmt.Errorf("release workspace sandbox process gate: %w", err), waitErr, cleanupErr)
		}
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr error
	select {
	case err := <-wait:
		waitErr = err
	case <-ctx.Done():
		// CommandContext only guarantees that the direct child is signaled. The
		// sandbox may have started helpers, so terminate the process group before
		// waiting for the child and all of its descendants to exit.
		termErr := signalProcessGroup(pid, syscall.SIGTERM)
		select {
		case err := <-wait:
			if termErr != nil {
				waitErr = errors.Join(err, fmt.Errorf("%w: terminate process group: %v", ErrCleanupFailed, termErr))
			} else {
				waitErr = err
			}
		case <-time.After(2 * time.Second):
			killErr := signalProcessGroup(pid, syscall.SIGKILL)
			if killErr != nil {
				waitErr = fmt.Errorf("%w: kill process group: %v", ErrCleanupFailed, killErr)
			} else {
				select {
				case err := <-wait:
					waitErr = err
				case <-time.After(2 * time.Second):
					waitErr = fmt.Errorf("%w: process group %d did not stop", ErrCleanupFailed, pid)
				}
			}
		}
	}
	if identity != nil {
		cleanupErr := stopLingeringProcessGroup(pid, 2*time.Second)
		if cleanupErr == nil {
			cleanupErr = onExit(*identity)
		}
		if cleanupErr != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("%w: tracked workspace process group cleanup: %v", ErrCleanupFailed, cleanupErr))
		}
	}
	return waitErr
}

func stopLingeringProcessGroup(pgid int, timeout time.Duration) error {
	active, err := proc.ProcessGroupActive(pgid)
	if err != nil || !active {
		return err
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if waitProcessGroupInactive(pgid, timeout) {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if waitProcessGroupInactive(pgid, timeout) {
		return nil
	}
	return fmt.Errorf("process group %d still has live members", pgid)
}

func waitProcessGroupInactive(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		active, err := proc.ProcessGroupActive(pgid)
		if err == nil && !active {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	active, err := proc.ProcessGroupActive(pgid)
	return err == nil && !active
}

// gateTrackedCommand starts a tiny host wrapper that waits on an inherited
// pipe before exec'ing the sandbox. This gives the parent time to persist the
// PID/start-time/token tuple while the untrusted command is unable to run.
func gateTrackedCommand(cmd *exec.Cmd) (release func() error, closeReader func(), cleanup func(), err error) {
	readGate, writeGate, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	path := cmd.Path
	args := append([]string(nil), cmd.Args...)
	if path == "" {
		_ = readGate.Close()
		_ = writeGate.Close()
		return nil, nil, nil, errors.New("tracked command path is empty")
	}
	if len(args) == 0 {
		args = []string{path}
	}
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"/bin/sh", "-c", "IFS= read -r _ <&3 || exit 125; exec \"$@\"", "stable-workspace-launch", path}, args[1:]...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, readGate)
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			_ = readGate.Close()
			_ = writeGate.Close()
		})
	}
	closeReader = func() { _ = readGate.Close() }
	release = func() error {
		_, writeErr := io.WriteString(writeGate, "go\n")
		_ = writeGate.Close()
		return writeErr
	}
	return release, closeReader, cleanup, nil
}

func withoutWorkspaceProcessToken(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if !strings.HasPrefix(item, "STABLE_WORKSPACE_PROCESS_TOKEN=") {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func waitForProxySocket(ctx context.Context, path string, done chan error) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case err := <-done:
			done <- err
			if err == nil {
				return errors.New("network proxy stopped before accepting connections")
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m LinuxManager) Probe(ctx context.Context, p SandboxProfile) error {
	probeProfile := p
	probeProfile.NetworkGrants = nil
	probeProfile.ProxyHelperPath = ""
	probeProfile.ReadOnlyFiles = nil
	if err := ValidateProfile(probeProfile); err != nil {
		return err
	}
	probeName := fmt.Sprintf(".stable-probe-%d", os.Getpid())
	if _, err := os.Lstat(filepath.Join(p.CandidateRoot, probeName)); err == nil {
		return errors.New("probe path already exists")
	}
	// Check the writable candidate and make sure the same host path is not
	// exposed. Network namespace creation is enforced by bwrap itself; the
	// Python socket check confirms that the resulting namespace has no route.
	privateSentinel := filepath.Join(filepath.Dir(p.RunRoot), ".stable-sandbox-probe-"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(privateSentinel, []byte("private"), 0600); err != nil {
		return err
	}
	defer os.Remove(privateSentinel)
	probeScript := "test -w /workspace/candidate && ! test -w /workspace/project && ! test -e \"$1\" && touch /workspace/candidate/" + probeName + " && rm /workspace/candidate/" + probeName
	check, err := m.run(ctx, probeProfile, []string{"/bin/sh", "-c", probeScript, "sandbox-probe", privateSentinel}, nil)
	if err != nil {
		return err
	}
	if check.ExitCode != 0 {
		return fmt.Errorf("filesystem isolation probe failed (exit %d): %s", check.ExitCode, strings.TrimSpace(string(check.Stderr)))
	}
	py := []string{"python3", "-c", "import socket\ns=socket.socket(); s.settimeout(1)\ntry:\n s.connect(('1.1.1.1',53))\n raise SystemExit(31)\nexcept OSError:\n pass\n"}
	res, err := m.run(ctx, probeProfile, py, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("network isolation probe failed (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
