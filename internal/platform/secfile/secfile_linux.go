//go:build linux

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func secureOpen(root, rel string) (*os.File, error) {
	if err := validateLinuxRelative(rel); err != nil {
		return nil, err
	}
	// O_NOFOLLOW applies to the root entry itself. Without it, a root
	// replaced between OpenRoot and this call could redirect the operation.
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapLinuxPathError(err)
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, rel, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, mapLinuxPathError(err)
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
		return nil, mapLinuxPathError(err)
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
	if err := unix.Lstat(pathA, &a); err != nil {
		return err
	}
	if err := unix.Lstat(pathB, &b); err != nil {
		return err
	}
	if a.Mode&unix.S_IFMT == unix.S_IFLNK || b.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrUnsafePath
	}
	if a.Dev != b.Dev {
		return ErrDifferentDevice
	}
	return nil
}

func moveDirectory(src, dst string, replace bool) error {
	for _, p := range []string{src, filepath.Dir(src)} {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	}
	if err := sameDevice(src, filepath.Dir(dst)); err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil {
		if !replace || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return os.ErrExist
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(src, dst)
}

func transactionMode() string { return "atomic-exchange" }

// validateLinuxRelative rejects path forms that openat2 would either resolve
// outside the root or interpret as an empty entry. Keeping this check local to
// the Linux adapter also lets callers receive the package sentinel instead of
// a platform-specific errno for clearly unsafe input.
func validateLinuxRelative(rel string) error {
	if rel == "" || strings.IndexByte(rel, 0) >= 0 || strings.HasPrefix(rel, "/") {
		return ErrUnsafePath
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return ErrUnsafePath
		}
	}
	return nil
}

func mapLinuxPathError(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return ErrUnsafePath
	}
	return err
}
