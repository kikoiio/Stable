//go:build !linux && !darwin && !windows

package secfile

import (
	"errors"
	"os"
)

func secureOpen(string, string) (*os.File, error) {
	return nil, errors.New("secfile: secure openat2 is only supported on Linux")
}

func openNoFollow(string) (*os.File, error) {
	return nil, errors.New("secfile: O_NOFOLLOW open is only supported on Linux")
}

func exchange(string, string) error {
	return errors.New("secfile: directory exchange is only supported on Linux")
}

func sameDevice(string, string) error {
	return ErrUnsupported
}

func moveDirectory(string, string, bool) error {
	return ErrUnsupported
}

func rootMkdirAll(string, string, os.FileMode) error { return ErrUnsupported }

func rootWriteFileAtomic(string, string, []byte, os.FileMode) error { return ErrUnsupported }

func rootRemoveFile(string, string) error { return ErrUnsupported }

func rootReadDir(string, string) ([]os.DirEntry, error) { return nil, ErrUnsupported }

func transactionMode() string { return "unsupported" }
