//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// terminate is shared by Linux and Darwin. Both platforms expose SIGTERM;
// Darwin has no Pdeathsig equivalent, so its group policy remains in the
// Darwin adapter.
func terminate(p *os.Process) error {
	if p == nil {
		return errors.New("proc: process is nil")
	}
	return p.Signal(syscall.SIGTERM)
}

// adoptChild is a no-op on Unix. Linux uses Pdeathsig in ConfigureChild and
// Darwin uses process groups because neither needs a post-start owner handle.
func adoptChild(*exec.Cmd) error { return nil }
