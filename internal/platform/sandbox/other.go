//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"io"
)

func New() SandboxManager {
	return LinuxManager{}
}

func newLinuxManager() LinuxManager {
	return LinuxManager{}
}

func (LinuxManager) executable() (string, error) {
	return "", fmt.Errorf("%w: Linux isolation is only supported on Linux", ErrUnavailable)
}

func (LinuxManager) args(SandboxProfile, []string) ([]string, error) {
	return nil, fmt.Errorf("%w: Linux isolation is only supported on Linux", ErrUnavailable)
}

func (LinuxManager) Probe(context.Context, SandboxProfile) error {
	return fmt.Errorf("%w: Linux isolation is only supported on Linux", ErrUnavailable)
}

func (LinuxManager) RunIsolated(context.Context, SandboxProfile, []string, io.Reader) (SandboxResult, error) {
	return SandboxResult{}, fmt.Errorf("%w: Linux isolation is only supported on Linux", ErrUnavailable)
}
