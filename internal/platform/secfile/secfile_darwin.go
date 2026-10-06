//go:build darwin

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Darwin does not provide openat2. Opening one component at a time with
// O_NOFOLLOW gives the same useful boundary: no component can redirect the
// lookup outside the already opened directory, and a symlink is rejected at
// the point where it is encountered.
func secureOpen(root, rel string) (*os.File, error) {
	parts, err := validateDarwinRelative(rel)
	if err != nil {
		return nil, err
	}

	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapDarwinPathError(err)
	}
	fd := rootFD
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(fd, part, flags, 0)
		if openErr != nil {
			return nil, mapDarwinPathError(openErr)
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Close(next)
			return nil, err
		}
		fd = next
	}

	file := os.NewFile(uintptr(fd), filepath.Join(root, rel))
	if file == nil {
		return nil, errors.New("secfile: unable to create file from descriptor")
	}
	closeFD = false
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrUnsafePath
	}
	return file, nil
}

func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapDarwinPathError(err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("secfile: unable to create file from descriptor")
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrUnsafePath
	}
	return file, nil
}

// Darwin has no renameat2(RENAME_EXCHANGE). Returning an explicit capability
// error lets the transaction layer select its journaled move path.
func exchange(dirA, dirB string) error {
	if _, err := validateDarwinDirectory(dirA, false); err != nil {
		return err
	}
	if _, err := validateDarwinDirectory(dirB, false); err != nil {
		return err
	}
	if err := sameDevice(dirA, dirB); err != nil {
		return err
	}
	return ErrUnsupported
}

// moveDirectory performs one same-volume directory rename. It deliberately
// does not emulate exchange: callers must journal the old root and choose the
// destination according to the requested transaction phase.
func moveDirectory(dirA, dirB string, replace bool) error {
	if _, err := validateDarwinDirectory(dirA, false); err != nil {
		return err
	}
	dst, err := validateDarwinDirectory(dirB, true)
	if err != nil {
		return err
	}
	if dst != nil && !replace {
		return os.ErrExist
	}
	dstParent := filepath.Dir(filepath.Clean(dirB))
	if _, err := validateDarwinDirectory(dstParent, false); err != nil {
		return err
	}
	if err := sameDevice(dirA, dstParent); err != nil {
		return err
	}
	if err := os.Rename(dirA, dirB); err != nil {
		return mapDarwinPathError(err)
	}
	return nil
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

func validateDarwinRelative(rel string) ([]string, error) {
	if rel == "" || strings.IndexByte(rel, 0) >= 0 || filepath.IsAbs(rel) {
		return nil, ErrUnsafePath
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

// validateDarwinDirectory walks every existing component with Lstat. This
// prevents a parent symlink from changing the destination of a later rename.
// A missing final component is allowed for moveDirectory's destination.
func validateDarwinDirectory(path string, allowMissingFinal bool) (os.FileInfo, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return nil, ErrUnsafePath
	}
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	parts := strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator))
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if allowMissingFinal && i == len(parts)-1 && errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, ErrUnsafePath
}

func mapDarwinPathError(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return ErrUnsafePath
	}
	if errors.Is(err, unix.EXDEV) {
		return ErrDifferentDevice
	}
	return err
}
