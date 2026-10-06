package runtime

import (
	"context"
	"errors"
	"io"
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
