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

var (
	// ErrUnavailable is kept as the top-level execution error so existing
	// callers continue to fail closed when any profile or capability check
	// fails. The more specific sentinels let callers distinguish the reason.
	ErrUnavailable               = errors.New("Linux isolation is unavailable")
	ErrProfileInvalid            = errors.New("sandbox profile is invalid")
	ErrNetworkGrantInvalid       = errors.New("sandbox network authorization is invalid")
	ErrSessionNetworkUnsupported = errors.New("persistent sandbox sessions do not support network authorization")
	ErrCleanupFailed             = errors.New("sandbox cleanup failed")
)

type SandboxProfile struct {
	ProjectRoot         string
	CandidateRoot       string
	RunRoot             string
	ReadOnlyMounts      []ReadOnlyMount
	ReadOnlyFiles       []ReadOnlyFileMount
	WorkspaceIsolation  bool
	WorkspaceVolumeRoot string
	Timeout             time.Duration
	OutputLimit         int
	Environment         []string
	NetworkGrants       []permission.NetworkGrant
	ProxyHelperPath     string
	CandidateID         string
	SessionID           string
	SessionArgv         []string
	sessionControl      bool
	sessionGeneration   uint64
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

func profileError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w: %w", ErrUnavailable, ErrProfileInvalid, err)
}

func profileMessage(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrUnavailable, ErrProfileInvalid, fmt.Sprintf(format, args...))
}

func networkGrantMessage(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrUnavailable, ErrNetworkGrantInvalid, fmt.Sprintf(format, args...))
}

func ValidateProfile(p SandboxProfile) error {
	for _, path := range []string{p.ProjectRoot, p.CandidateRoot, p.RunRoot} {
		if path == "" {
			return profileMessage("project, candidate and private run roots are required")
		}
		if !filepath.IsAbs(path) {
			return profileMessage("sandbox roots must be absolute: %q", path)
		}
	}
	project, err := filepath.Abs(p.ProjectRoot)
	if err != nil {
		return profileError(err)
	}
	candidate, err := filepath.Abs(p.CandidateRoot)
	if err != nil {
		return profileError(err)
	}
	run, err := filepath.Abs(p.RunRoot)
	if err != nil {
		return profileError(err)
	}
	for i, root := range []string{project, candidate, run} {
		info, err := os.Lstat(root)
		if err != nil {
			return profileError(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return profileMessage("%s is not a directory", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(root) {
			return profileMessage("%s resolves through a symbolic link", root)
		}
		if i == 2 && info.Mode().Perm()&0077 != 0 {
			return profileMessage("private run root %s is accessible by another user", root)
		}
	}
	if project == candidate || project == run || candidate == run {
		return profileMessage("roots must be distinct")
	}
	for i, root := range []string{project, candidate, run} {
		for _, other := range []string{project, candidate, run}[i+1:] {
			if pathContains(root, other) || pathContains(other, root) {
				return profileMessage("project, candidate, and private run roots must not overlap")
			}
		}
	}
	if p.WorkspaceVolumeRoot != "" {
		if !p.WorkspaceIsolation {
			return profileMessage("volume boundary requires workspace isolation")
		}
		if err := BoundedWorkspaceVolume(p.WorkspaceVolumeRoot, p.ProjectRoot, p.CandidateRoot, p.RunRoot); err != nil {
			return err
		}
	}
	if p.WorkspaceIsolation && len(p.NetworkGrants) != 0 {
		return networkGrantMessage("workspace commands cannot access network")
	}
	if p.Timeout < 0 {
		return profileMessage("sandbox timeout cannot be negative")
	}
	if p.OutputLimit < 0 {
		return profileMessage("sandbox output limit cannot be negative")
	}
	seenEnv := make(map[string]struct{}, len(p.Environment))
	for _, entry := range p.Environment {
		if strings.ContainsRune(entry, '\x00') || !strings.Contains(entry, "=") {
			return profileMessage("invalid sandbox environment entry")
		}
		key := strings.SplitN(entry, "=", 2)[0]
		if key == "" || strings.TrimSpace(key) != key || !validEnvironmentKey(key) {
			return profileMessage("invalid sandbox environment variable name %q", key)
		}
		upperKey := strings.ToUpper(key)
		if _, exists := seenEnv[upperKey]; exists {
			return profileMessage("duplicate sandbox environment variable %q", key)
		}
		seenEnv[upperKey] = struct{}{}
		if upperKey == "HOME" || upperKey == "PATH" || upperKey == "DISPLAY" || strings.Contains(upperKey, "KEY") || strings.Contains(upperKey, "TOKEN") || strings.HasPrefix(upperKey, "STABLE_SESSION_") {
			return profileMessage("environment variable %q cannot be passed to sandbox", key)
		}
	}
	seenGuest := make(map[string]struct{}, len(p.ReadOnlyMounts)+len(p.ReadOnlyFiles))
	for _, mount := range p.ReadOnlyMounts {
		guest := mount.GuestPath
		validGuest := strings.HasPrefix(guest, "/workspace/") || (guest == "/run/stable-network" && len(p.NetworkGrants) > 0) || (guest == "/run/stable-session" && p.sessionControl)
		if !filepath.IsAbs(mount.HostPath) || !validGuest || guest == "/workspace/" || filepath.Clean(guest) != guest || strings.Contains(guest, "..") || strings.ContainsRune(guest, 0) {
			return profileMessage("invalid read-only sandbox mount")
		}
		if _, exists := seenGuest[guest]; exists {
			return profileMessage("duplicate sandbox mount %q", guest)
		}
		seenGuest[guest] = struct{}{}
		info, err := os.Lstat(mount.HostPath)
		if err != nil {
			return profileError(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return profileMessage("sandbox runtime mount cannot be a symlink")
		}
		if !info.IsDir() {
			return profileMessage("sandbox runtime mount must be a directory")
		}
		resolved, err := filepath.EvalSymlinks(mount.HostPath)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(mount.HostPath) {
			return profileMessage("sandbox runtime mount crosses a symbolic link")
		}
	}
	for _, mount := range p.ReadOnlyFiles {
		if !filepath.IsAbs(mount.HostPath) || mount.GuestPath != "/workspace/runtime/agentworker" {
			return profileMessage("invalid read-only sandbox file mount")
		}
		if _, exists := seenGuest[mount.GuestPath]; exists {
			return profileMessage("duplicate sandbox mount %q", mount.GuestPath)
		}
		seenGuest[mount.GuestPath] = struct{}{}
		info, err := os.Lstat(mount.HostPath)
		if err != nil {
			return profileError(err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0111 == 0 {
			return profileMessage("sandbox helper must be a regular executable file")
		}
		resolved, err := filepath.EvalSymlinks(mount.HostPath)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(mount.HostPath) {
			return profileMessage("sandbox helper path crosses a symbolic link")
		}
	}
	if len(p.NetworkGrants) > 0 {
		foundHelper := false
		for _, mount := range p.ReadOnlyFiles {
			foundHelper = foundHelper || (mount.HostPath == p.ProxyHelperPath && mount.GuestPath == "/workspace/runtime/agentworker")
		}
		if p.ProxyHelperPath == "" || !foundHelper {
			return networkGrantMessage("network proxy helper is unavailable")
		}
		if p.sessionControl {
			return networkGrantMessage("network authorization cannot be used by a persistent session")
		}
		seen := map[string]bool{}
		for _, grant := range p.NetworkGrants {
			host, err := normalizeHost(grant.Host)
			ips, ipErr := normalizedIPs(grant.ResolvedIPs)
			if err != nil || ipErr != nil || grant.Protocol != "tcp" || grant.Host != host || grant.Port == 0 || len(ips) == 0 {
				return networkGrantMessage("network grant is incomplete or unpinned")
			}
			key := fmt.Sprintf("%s:%s:%d", grant.Protocol, host, grant.Port)
			if seen[key] {
				return networkGrantMessage("duplicate network grant")
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
			return profileMessage("isolated session control socket is unavailable")
		}
	}
	return nil
}

func validEnvironmentKey(key string) bool {
	for i, char := range key {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (i > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return key != ""
}

func pathContains(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
