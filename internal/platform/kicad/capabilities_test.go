package kicad

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/platform/sandbox"
)

type capabilitySandboxFake struct {
	probeErr error
	runErr   error
	exitCode int
	runCalls [][]string
	version  string
}

func (f *capabilitySandboxFake) Probe(context.Context, sandbox.SandboxProfile) error {
	return f.probeErr
}

func (f *capabilitySandboxFake) RunIsolated(_ context.Context, _ sandbox.SandboxProfile, argv []string, _ io.Reader) (sandbox.SandboxResult, error) {
	f.runCalls = append(f.runCalls, append([]string(nil), argv...))
	if f.runErr != nil {
		return sandbox.SandboxResult{}, f.runErr
	}
	return sandbox.SandboxResult{ExitCode: f.exitCode, Stdout: []byte(f.version)}, nil
}

func (*capabilitySandboxFake) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("not used")
}
func (*capabilitySandboxFake) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("not used")
}
func (*capabilitySandboxFake) StopIsolatedSession(context.Context, string) error {
	return errors.New("not used")
}

func fakeCapabilityEnvironment(t *testing.T) {
	t.Helper()
	originalLookPath := kicadLookPath
	originalTemplates := kicadTemplateSearch
	kicadLookPath = func(name string) (string, error) {
		if name == "missing" {
			return "", errors.New("not found")
		}
		return filepath.Join("/usr/bin", name), nil
	}
	kicadTemplateSearch = func() []string { return []string{} }
	t.Cleanup(func() {
		kicadLookPath = originalLookPath
		kicadTemplateSearch = originalTemplates
	})
}

func TestEnvironmentUsesPrivateRunDirectories(t *testing.T) {
	env := Environment("/tmp/stable-run")
	joined := strings.Join(env, "\n")
	for _, expected := range []string{
		"XDG_CONFIG_HOME=/tmp/stable-run/config",
		"XDG_CACHE_HOME=/tmp/stable-run/cache",
		"XDG_DATA_HOME=/tmp/stable-run/data",
		"KICAD_CONFIG_HOME=/tmp/stable-run/config/kicad",
		"KICAD_USER_TEMPLATE_DIR=/tmp/stable-run/templates",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("environment missing %q: %v", expected, env)
		}
	}
	if got := (Capabilities{}).Environment("/tmp/stable-run"); len(got) != len(env) {
		t.Fatalf("method environment length = %d, package environment length = %d", len(got), len(env))
	}
}

func TestRequiredForAndMissing(t *testing.T) {
	if got := RequiredFor("erc"); len(got) == 0 {
		t.Fatal("ERC requirements are empty")
	}
	capabilities := Capabilities{Sandbox: SandboxCapability{Available: true}, Python: ToolStatus{Available: true}, CLI: ToolStatus{Available: true}, Template: ToolStatus{Available: true}, TemplateRoot: "/templates"}
	if !capabilities.AvailableFor("erc") {
		t.Fatalf("complete ERC capabilities reported missing: %v", capabilities.Missing("erc"))
	}
	if capabilities.AvailableFor("unknown") || len(capabilities.Missing("unknown")) != 0 {
		t.Fatal("unknown capability kind should fail closed without a misleading missing list")
	}
}

func TestDiscoverReportsIsolatedToolchain(t *testing.T) {
	fakeCapabilityEnvironment(t)
	kicadTemplateSearch = func() []string { return []string{"/tmp"} }
	fake := &capabilitySandboxFake{version: "KiCad 9.0\n"}
	capabilities, err := Discover(context.Background(), fake, sandbox.SandboxProfile{})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []ToolStatus{capabilities.Python, capabilities.CLI, capabilities.GUI, capabilities.Display, capabilities.Window, capabilities.Screenshot, capabilities.Template} {
		if !status.Available || !status.HostAvailable || !status.IsolatedAvailable {
			t.Errorf("capability %q is not available: %+v", status.Name, status)
		}
	}
	if capabilities.TemplateRoot != "/tmp" {
		t.Fatalf("template root = %q, want /tmp", capabilities.TemplateRoot)
	}
	if len(fake.runCalls) < 8 {
		t.Fatalf("isolated probes = %d, want tool and template probes", len(fake.runCalls))
	}
}

func TestDiscoverDoesNotReportHostOnlyTools(t *testing.T) {
	fakeCapabilityEnvironment(t)
	fake := &capabilitySandboxFake{exitCode: 127}
	capabilities, err := Discover(context.Background(), fake, sandbox.SandboxProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.CLI.HostAvailable != true || capabilities.CLI.IsolatedAvailable || capabilities.CLI.Available {
		t.Fatalf("host-only CLI was reported usable: %+v", capabilities.CLI)
	}
	if !strings.Contains(capabilities.CLI.Detail, "isolated sandbox") {
		t.Fatalf("CLI detail lacks isolation reason: %q", capabilities.CLI.Detail)
	}
}

func TestDiscoverReportsSandboxFailureAndMissingTemplate(t *testing.T) {
	fakeCapabilityEnvironment(t)
	fake := &capabilitySandboxFake{probeErr: errors.New("bwrap unavailable")}
	capabilities, err := Discover(context.Background(), fake, sandbox.SandboxProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Sandbox.Available || !strings.Contains(capabilities.Sandbox.Detail, "bwrap unavailable") {
		t.Fatalf("sandbox failure was not diagnosed: %+v", capabilities.Sandbox)
	}
	if capabilities.Python.HostAvailable != true || capabilities.Python.Available {
		t.Fatalf("host tool became usable after sandbox failure: %+v", capabilities.Python)
	}
	if capabilities.Template.HostAvailable || capabilities.TemplateRoot != "" {
		t.Fatalf("missing template was reported as available: %+v root=%q", capabilities.Template, capabilities.TemplateRoot)
	}
}

func TestDiscoverReportsMissingHostTool(t *testing.T) {
	fakeCapabilityEnvironment(t)
	kicadLookPath = func(name string) (string, error) {
		if name == "kicad-cli" {
			return "", errors.New("not found")
		}
		return filepath.Join("/usr/bin", name), nil
	}
	fake := &capabilitySandboxFake{version: "ok"}
	capabilities, err := Discover(context.Background(), fake, sandbox.SandboxProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.CLI.HostAvailable || capabilities.CLI.Available || !strings.Contains(capabilities.CLI.Detail, "not available on host") {
		t.Fatalf("missing host CLI was not diagnosed: %+v", capabilities.CLI)
	}
}
