//go:build linux || darwin

package lock

import (
	"os"
	"syscall"

	"stable/internal/platform/secfile"
)

type fileGuard struct {
	file *os.File
}

func tryAcquire(path string) (Guard, error) {
	file, err := secfile.OpenFilePrivate(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return &fileGuard{file: file}, nil
}

func (g *fileGuard) Release() error {
	if g == nil || g.file == nil {
		return nil
	}
	_ = syscall.Flock(int(g.file.Fd()), syscall.LOCK_UN)
	err := g.file.Close()
	g.file = nil
	return err
}
