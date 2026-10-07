//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/permission"
)

func sandboxFixture(t *testing.T) SandboxProfile {
	t.Helper()
	root := t.TempDir()
	p := SandboxProfile{ProjectRoot: filepath.Join(root, "project"), CandidateRoot: filepath.Join(root, "candidate"), RunRoot: filepath.Join(root, "run")}
	for _, d := range []string{p.ProjectRoot, p.CandidateRoot, p.RunRoot} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestProfileValidation(t *testing.T) {
	p := sandboxFixture(t)
	if err := ValidateProfile(p); err != nil {
		t.Fatal(err)
	}
	p.CandidateRoot = ""
	if err := ValidateProfile(p); err == nil {
		t.Fatal("missing candidate root accepted")
	}
	p = sandboxFixture(t)
	p.Environment = []string{"STABLE_API_KEY=secret"}
	if err := ValidateProfile(p); err == nil {
		t.Fatal("secret environment accepted")
	}
	p = sandboxFixture(t)
	p.Environment = []string{"home=/tmp"}
	if err := ValidateProfile(p); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("lowercase sensitive environment was not classified: %v", err)
	}
	p = sandboxFixture(t)
	p.Environment = []string{"XDG_CACHE_HOME=/tmp/cache", "xdg_cache_home=/tmp/other"}
	if err := ValidateProfile(p); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("duplicate environment was not classified: %v", err)
	}
	p = sandboxFixture(t)
	p.ProjectRoot = "relative-project"
	if err := ValidateProfile(p); !errors.Is(err, ErrProfileInvalid) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative root was not rejected as unavailable: %v", err)
	}
	p = sandboxFixture(t)
	if err := os.Chmod(p.RunRoot, 0750); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProfile(p); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("non-private run root was accepted: %v", err)
	}
}

func TestProfileRejectsDuplicateMountAndUnpinnedGrant(t *testing.T) {
	p := sandboxFixture(t)
	mount := filepath.Join(filepath.Dir(p.RunRoot), "runtime")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	p.ReadOnlyMounts = []ReadOnlyMount{
		{HostPath: mount, GuestPath: "/workspace/runtime"},
		{HostPath: mount, GuestPath: "/workspace/runtime"},
	}
	if err := ValidateProfile(p); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("duplicate mount was accepted: %v", err)
	}

	p = sandboxFixture(t)
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p.NetworkGrants = []permission.NetworkGrant{{Protocol: "tcp", Host: "target.test", Port: 443}}
	p.ProxyHelperPath = helper
	p.ReadOnlyFiles = []ReadOnlyFileMount{{HostPath: helper, GuestPath: "/workspace/runtime/agentworker"}}
	if err := ValidateProfile(p); !errors.Is(err, ErrNetworkGrantInvalid) {
		t.Fatalf("unpinned grant was accepted: %v", err)
	}
}

// bwrapPath returns the bubblewrap binary path, skipping the test on hosts
// without it (CI runners); argument-construction coverage runs wherever the
// real binary exists.
func bwrapPath(t *testing.T) string {
	t.Helper()
	const path = "/usr/bin/bwrap"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("bubblewrap not installed on this host: %v", err)
	}
	return path
}

func TestBubblewrapArgs(t *testing.T) {
	p := sandboxFixture(t)
	m := LinuxManager{Bwrap: bwrapPath(t)}
	a, err := m.args(p, []string{"python3", "-c", "pass"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(a, " ")
	for _, want := range []string{"--unshare-pid", "--unshare-net", "--clearenv", "--ro-bind " + p.ProjectRoot, "--bind " + p.CandidateRoot, "--bind " + p.RunRoot} {
		if !strings.Contains(joined, want) {
			t.Errorf("bubblewrap args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--bind "+p.ProjectRoot) {
		t.Fatal("project is writable")
	}
}

func TestBubblewrapArgsWithApprovedNetworkProxy(t *testing.T) {
	p := sandboxFixture(t)
	root := filepath.Dir(p.RunRoot)
	proxyDir := filepath.Join(root, "proxy")
	if err := os.Mkdir(proxyDir, 0700); err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p.NetworkGrants = []permission.NetworkGrant{{Protocol: "tcp", Host: "target.test", Port: 443, ResolvedIPs: []string{"127.0.0.1"}}}
	p.ProxyHelperPath = helper
	p.ReadOnlyFiles = []ReadOnlyFileMount{{HostPath: helper, GuestPath: "/workspace/runtime/agentworker"}}
	p.ReadOnlyMounts = []ReadOnlyMount{{HostPath: proxyDir, GuestPath: "/run/stable-network"}}
	a, err := (LinuxManager{Bwrap: bwrapPath(t)}).args(p, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(a, " ")
	for _, want := range []string{"--unshare-net", "--ro-bind " + proxyDir + " /run/stable-network", "--ro-bind " + helper + " /workspace/runtime/agentworker"} {
		if !strings.Contains(joined, want) {
			t.Errorf("network sandbox args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--share-net") || strings.Contains(joined, "--share-pid") {
		t.Fatalf("network grant disabled namespace isolation: %s", joined)
	}
}

func TestBubblewrapArgsWithSessionControl(t *testing.T) {
	p := sandboxFixture(t)
	controlDir := filepath.Join(filepath.Dir(p.RunRoot), "session-control")
	if err := os.Mkdir(controlDir, 0700); err != nil {
		t.Fatal(err)
	}
	p.CandidateID = "candidate-1"
	p.SessionID = "session-1"
	p.SessionArgv = []string{"python3", "/workspace/bridge/bridge.py"}
	p.sessionControl = true
	p.sessionGeneration = 3
	p.ReadOnlyMounts = []ReadOnlyMount{{HostPath: controlDir, GuestPath: "/run/stable-session"}}
	a, err := (LinuxManager{Bwrap: bwrapPath(t)}).args(p, p.SessionArgv)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(a, " ")
	for _, want := range []string{"--unshare-pid", "--unshare-net", "--ro-bind " + controlDir + " /run/stable-session", "STABLE_SESSION_SOCKET /run/stable-session/session.sock", "STABLE_SESSION_GENERATION 3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("session sandbox args missing %q: %s", want, joined)
		}
	}
}

func TestProbeAndNoHostFallback(t *testing.T) {
	p := sandboxFixture(t)
	sentinel := filepath.Join(p.ProjectRoot, "sentinel")
	if err := os.WriteFile(sentinel, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	m := LinuxManager{Bwrap: "/usr/bin/bwrap"}
	err := m.Probe(context.Background(), p)
	if err != nil {
		t.Logf("Linux isolation probe unavailable on this host: %v", err)
		if _, statErr := os.Stat(filepath.Join(p.CandidateRoot, "host-write")); !os.IsNotExist(statErr) {
			t.Fatalf("probe created host artifact: %v", statErr)
		}
		return // constrained hosts must fail closed
	}
	result, runErr := m.RunIsolated(context.Background(), p, []string{"/bin/sh", "-c", "cat /workspace/project/sentinel; touch /workspace/candidate/ok"}, nil)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "original" {
		t.Fatalf("isolated command result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(p.CandidateRoot, "ok")); err != nil {
		t.Fatal("candidate write missing:", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "original" {
		t.Fatalf("project changed: %q %v", got, err)
	}
}

func TestWorkspaceSandboxMasksPrivateMetadata(t *testing.T) {
	p := sandboxFixture(t)
	if err := os.WriteFile(filepath.Join(p.CandidateRoot, ".git"), []byte("gitdir: service-private"), 0600); err != nil {
		t.Fatal(err)
	}
	p.WorkspaceIsolation = true
	args, err := (LinuxManager{Bwrap: bwrapPath(t)}).args(p, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--cap-drop ALL", "--ro-bind /dev/null /workspace/candidate/.git", "--tmpfs /workspace/candidate/.stable --remount-ro /workspace/candidate/.stable", "--tmpfs /workspace/candidate/.mewcode --remount-ro /workspace/candidate/.mewcode"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing mask %s: %s", want, joined)
		}
	}
}
