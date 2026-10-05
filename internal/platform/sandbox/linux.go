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

func New() SandboxManager {
	return LinuxManager{}
}

func newLinuxManager() LinuxManager {
	return LinuxManager{}
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
