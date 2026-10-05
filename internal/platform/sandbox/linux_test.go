//go:build linux

package sandbox

import (
	"context"
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
