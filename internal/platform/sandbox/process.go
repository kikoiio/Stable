package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
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
