//go:build linux

package secfile

import (
	"os"

	"golang.org/x/sys/unix"
)

func secureOpen(root, rel string) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), rel)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrUnsafePath
	}
	return file, nil
}

func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func exchange(dirA, dirB string) error {
	for _, p := range []string{dirA, dirB} {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	}
	if err := sameDevice(dirA, dirB); err != nil {
		return err
	}
	return unix.Renameat2(unix.AT_FDCWD, dirA, unix.AT_FDCWD, dirB, unix.RENAME_EXCHANGE)
}

func sameDevice(pathA, pathB string) error {
	var a, b unix.Stat_t
	if err := unix.Stat(pathA, &a); err != nil {
		return err
	}
	if err := unix.Stat(pathB, &b); err != nil {
		return err
	}
	if a.Dev != b.Dev {
		return ErrDifferentDevice
	}
	return nil
}
