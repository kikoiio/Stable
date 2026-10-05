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
	"syscall"
	"time"
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
		return SandboxResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return m.run(ctx, p, argv, stdin)
}

func (m LinuxManager) run(ctx context.Context, p SandboxProfile, argv []string, stdin io.Reader) (SandboxResult, error) {
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
		if proxyCancel != nil {
			proxyCancel()
			if proxyDone != nil {
				<-proxyDone
			}
		}
		if proxyDir != "" {
			_ = os.RemoveAll(proxyDir)
		}
	}()
	if len(p.NetworkGrants) > 0 {
		if p.ProxyHelperPath == "" {
			return SandboxResult{}, fmt.Errorf("%w: network proxy helper is unavailable", ErrUnavailable)
		}
		runRoot, err := filepath.Abs(p.RunRoot)
		if err != nil {
			return SandboxResult{}, err
		}
		proxyDir, err = os.MkdirTemp(filepath.Dir(runRoot), ".stable-network-")
		if err != nil {
			return SandboxResult{}, fmt.Errorf("create private proxy directory: %w", err)
		}
		if err = os.Chmod(proxyDir, 0700); err != nil {
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
			return SandboxResult{}, fmt.Errorf("%w: start network proxy: %v", ErrUnavailable, err)
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
	cmd.Stdin = stdin
	stdout := &limitedBuffer{limit: p.OutputLimit}
	stderr := &limitedBuffer{limit: p.OutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = runProcessGroup(runCtx, cmd)
	result := SandboxResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return result, runCtx.Err()
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
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case err := <-wait:
		return err
	case <-ctx.Done():
		// CommandContext only guarantees that the direct child is signaled. The
		// sandbox may have started helpers, so terminate the process group before
		// waiting for the child and all of its descendants to exit.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case err := <-wait:
			return err
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return <-wait
		}
	}
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
