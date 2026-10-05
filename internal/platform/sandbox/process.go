package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stable/internal/permission"
)

var ErrUnavailable = errors.New("Linux isolation is unavailable")

type SandboxProfile struct {
	ProjectRoot       string
	CandidateRoot     string
	RunRoot           string
	ReadOnlyMounts    []ReadOnlyMount
	ReadOnlyFiles     []ReadOnlyFileMount
	Timeout           time.Duration
	OutputLimit       int
	Environment       []string
	NetworkGrants     []permission.NetworkGrant
	ProxyHelperPath   string
	CandidateID       string
	SessionID         string
	SessionArgv       []string
	sessionControl    bool
	sessionGeneration uint64
}

type ReadOnlyMount struct {
	HostPath  string
	GuestPath string
}

type ReadOnlyFileMount struct {
	HostPath  string
	GuestPath string
}

type SandboxResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

type SandboxSession struct {
	ID          string
	Generation  uint64
	CandidateID string
	PID         int
}

type SandboxManager interface {
	Probe(context.Context, SandboxProfile) error
	RunIsolated(context.Context, SandboxProfile, []string, io.Reader) (SandboxResult, error)
	StartIsolatedSession(context.Context, SandboxProfile) (SandboxSession, error)
	CallIsolatedSession(context.Context, SandboxSession, io.Reader) (SandboxResult, error)
	StopIsolatedSession(context.Context, string) error
}

type CommandFactory func(context.Context, string, ...string) *exec.Cmd

type LinuxManager struct {
	Bwrap      string
	NewCommand CommandFactory
}

func (m LinuxManager) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if m.NewCommand != nil {
		return m.NewCommand(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func ValidateProfile(p SandboxProfile) error {
	for _, path := range []string{p.ProjectRoot, p.CandidateRoot, p.RunRoot} {
		if path == "" {
			return fmt.Errorf("%w: project, candidate and private run roots are required", ErrUnavailable)
		}
	}
	project, err := filepath.Abs(p.ProjectRoot)
	if err != nil {
		return err
	}
	candidate, err := filepath.Abs(p.CandidateRoot)
	if err != nil {
		return err
	}
	run, err := filepath.Abs(p.RunRoot)
	if err != nil {
		return err
	}
	for _, root := range []string{project, candidate, run} {
		info, err := os.Lstat(root)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a directory", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(root) {
			return fmt.Errorf("%s resolves through a symbolic link", root)
		}
	}
	if project == candidate || project == run || candidate == run {
		return fmt.Errorf("%w: roots must be distinct", ErrUnavailable)
	}
	for i, root := range []string{project, candidate, run} {
		for _, other := range []string{project, candidate, run}[i+1:] {
			if pathContains(root, other) || pathContains(other, root) {
				return fmt.Errorf("%w: project, candidate, and private run roots must not overlap", ErrUnavailable)
			}
		}
	}
	for _, entry := range p.Environment {
		if strings.ContainsRune(entry, '\x00') || !strings.Contains(entry, "=") {
			return fmt.Errorf("invalid sandbox environment entry")
		}
		key := strings.SplitN(entry, "=", 2)[0]
		upperKey := strings.ToUpper(key)
		if key == "HOME" || key == "PATH" || key == "DISPLAY" || strings.Contains(upperKey, "KEY") || strings.Contains(upperKey, "TOKEN") || strings.HasPrefix(upperKey, "STABLE_SESSION_") {
			return fmt.Errorf("environment variable %q cannot be passed to sandbox", key)
		}
	}
	for _, mount := range p.ReadOnlyMounts {
		validGuest := strings.HasPrefix(mount.GuestPath, "/workspace/") || (mount.GuestPath == "/run/stable-network" && len(p.NetworkGrants) > 0) || (mount.GuestPath == "/run/stable-session" && p.sessionControl)
		if !filepath.IsAbs(mount.HostPath) || !validGuest || strings.Contains(mount.GuestPath, "..") || strings.ContainsRune(mount.GuestPath, 0) {
			return fmt.Errorf("invalid read-only sandbox mount")
		}
		info, err := os.Lstat(mount.HostPath)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sandbox runtime mount cannot be a symlink")
		}
		if !info.IsDir() {
			return fmt.Errorf("sandbox runtime mount must be a directory")
		}
		resolved, err := filepath.EvalSymlinks(mount.HostPath)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(mount.HostPath) {
			return fmt.Errorf("sandbox runtime mount crosses a symbolic link")
		}
	}
	for _, mount := range p.ReadOnlyFiles {
		if len(p.NetworkGrants) == 0 || !filepath.IsAbs(mount.HostPath) || mount.GuestPath != "/workspace/runtime/agentworker" {
			return fmt.Errorf("invalid read-only sandbox file mount")
		}
		info, err := os.Lstat(mount.HostPath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("sandbox helper must be a regular executable file")
		}
		resolved, err := filepath.EvalSymlinks(mount.HostPath)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(mount.HostPath) {
			return fmt.Errorf("sandbox helper path crosses a symbolic link")
		}
	}
	if len(p.NetworkGrants) > 0 {
		foundHelper := false
		for _, mount := range p.ReadOnlyFiles {
			foundHelper = foundHelper || (mount.HostPath == p.ProxyHelperPath && mount.GuestPath == "/workspace/runtime/agentworker")
		}
		if p.ProxyHelperPath == "" || !foundHelper {
			return fmt.Errorf("network proxy helper is unavailable")
		}
		seen := map[string]bool{}
		for _, grant := range p.NetworkGrants {
			host, err := normalizeHost(grant.Host)
			ips, ipErr := normalizedIPs(grant.ResolvedIPs)
			if err != nil || ipErr != nil || grant.Protocol != "tcp" || grant.Port == 0 || len(ips) == 0 {
				return fmt.Errorf("network grant is incomplete or unpinned")
			}
			key := fmt.Sprintf("%s:%s:%d", grant.Protocol, host, grant.Port)
			if seen[key] {
				return fmt.Errorf("duplicate network grant")
			}
			seen[key] = true
		}
	}
	if p.sessionControl {
		foundControl := false
		for _, mount := range p.ReadOnlyMounts {
			foundControl = foundControl || mount.GuestPath == "/run/stable-session"
		}
		if !foundControl || p.sessionGeneration == 0 || len(p.SessionArgv) == 0 {
			return fmt.Errorf("isolated session control socket is unavailable")
		}
	}
	return nil
}

func pathContains(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
