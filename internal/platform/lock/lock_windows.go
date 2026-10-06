//go:build windows

package lock

import (
	"os"

	"golang.org/x/sys/windows"
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
	if err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, nil); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &fileGuard{file: file}, nil
}

func (g *fileGuard) Release() error {
	if g == nil || g.file == nil {
		return nil
	}
	_ = windows.UnlockFileEx(windows.Handle(g.file.Fd()), 0, 1, 0, nil)
	err := g.file.Close()
	g.file = nil
	return err
}
