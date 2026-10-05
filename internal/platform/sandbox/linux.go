//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type LinuxManager struct {
	Bwrap      string
	NewCommand CommandFactory
}

func (m LinuxManager) executable() (string, error) {
	if m.Bwrap != "" {
		if _, err := os.Stat(m.Bwrap); err != nil {
			return "", err
		}
		return m.Bwrap, nil
	}
	p, err := exec.LookPath("bwrap")
	if err != nil {
		return "", fmt.Errorf("%w: bwrap not found", ErrUnavailable)
	}
	return p, nil
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

func (m LinuxManager) args(p SandboxProfile, argv []string) ([]string, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("sandbox argv is empty")
	}
	bwrap, err := m.executable()
	if err != nil {
		return nil, err
	}
	project, err := filepath.Abs(p.ProjectRoot)
	if err != nil {
		return nil, err
	}
	candidate, err := filepath.Abs(p.CandidateRoot)
	if err != nil {
		return nil, err
	}
	run, err := filepath.Abs(p.RunRoot)
	if err != nil {
		return nil, err
	}
	// /tmp/.X11-unix must exist before Xvfb starts: as a non-root guest Xvfb
	// will not create the directory itself and exits.
	args := []string{"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-net", "--clearenv", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/tmp/.X11-unix", "--tmpfs", "/home", "--dir", "/workspace", "--dir", "/workspace/project", "--dir", "/workspace/candidate", "--dir", "/workspace/run", "--ro-bind", project, "/workspace/project", "--bind", candidate, "/workspace/candidate", "--bind", run, "/workspace/run"}
	if len(p.NetworkGrants) > 0 || p.sessionControl {
		args = append(args, "--dir", "/run", "--dir", "/workspace/runtime")
	}
	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc/ssl", "/etc/ld.so.cache"} {
		if _, e := os.Lstat(path); e == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, mount := range p.ReadOnlyMounts {
		args = append(args, "--dir", mount.GuestPath, "--ro-bind", mount.HostPath, mount.GuestPath)
	}
	for _, mount := range p.ReadOnlyFiles {
		args = append(args, "--ro-bind", mount.HostPath, mount.GuestPath)
	}
	args = append(args, "--chdir", "/workspace/candidate", "--setenv", "HOME", "/tmp", "--setenv", "TMPDIR", "/tmp", "--setenv", "PATH", "/usr/bin:/bin")
	if p.sessionControl {
		args = append(args, "--setenv", "STABLE_SESSION_SOCKET", "/run/stable-session/session.sock", "--setenv", "STABLE_SESSION_GENERATION", strconv.FormatUint(p.sessionGeneration, 10))
	}
	for _, entry := range p.Environment {
		kv := strings.SplitN(entry, "=", 2)
		args = append(args, "--setenv", kv[0], kv[1])
	}
	args = append(args, "--")
	for _, arg := range argv {
		if strings.ContainsRune(arg, '\x00') {
			return nil, fmt.Errorf("argv contains NUL")
		}
	}
	return append([]string{bwrap}, append(args, argv...)...), nil
}

func pathContains(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
