package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/platform/sandbox"
)

type doctorSandbox struct{}

func (doctorSandbox) Probe(context.Context, sandbox.SandboxProfile) error {
	return errors.New("bwrap unavailable for doctor test")
}
func (doctorSandbox) RunIsolated(context.Context, sandbox.SandboxProfile, []string, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("probe must fail before isolated command")
}
func (doctorSandbox) StartIsolatedSession(context.Context, sandbox.SandboxProfile) (sandbox.SandboxSession, error) {
	return sandbox.SandboxSession{}, errors.New("not used")
}
func (doctorSandbox) CallIsolatedSession(context.Context, sandbox.SandboxSession, io.Reader) (sandbox.SandboxResult, error) {
	return sandbox.SandboxResult{}, errors.New("not used")
}
func (doctorSandbox) StopIsolatedSession(context.Context, string) error { return nil }

func TestDoctorReportsIsolatedCapabilityFailureWithoutHostFallback(t *testing.T) {
	// The report only falls through to the isolated-sandbox wording when the
	// CLI is visible on the host, so give the test a hermetic stub instead of
	// depending on a host KiCad install (CI runners have none).
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "kicad-cli")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	checks := DoctorWithSandbox(appconfig.AppConfig{}, paths.Paths{}, doctorSandbox{})
	var sandboxCheck, cliCheck *Check
	for i := range checks {
		if checks[i].Name == "sandbox (isolated)" {
			sandboxCheck = &checks[i]
		}
		if checks[i].Name == "kicad-cli (isolated)" {
			cliCheck = &checks[i]
		}
	}
	if sandboxCheck == nil || sandboxCheck.OK || !strings.Contains(sandboxCheck.Detail, "bwrap unavailable") {
		t.Fatalf("sandbox capability was not diagnosed: %+v", sandboxCheck)
	}
	if cliCheck == nil || cliCheck.OK || !strings.Contains(cliCheck.Detail, "isolated sandbox") {
		t.Fatalf("host-visible CLI was reported usable: %+v", cliCheck)
	}
}
