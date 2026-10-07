//go:build darwin

package secfile

import (
	"crypto/rand"
	"encoding/hex"
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

func transactionMode() string { return "journaled-move" }

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

func rootMkdirAll(root, rel string, perm os.FileMode) error {
	parts, err := validateDarwinRelative(rel)
	if err != nil {
		return err
	}
	fd, err := openDarwinRootDir(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts {
		created := true
		if err := unix.Mkdirat(fd, part, uint32(perm)); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return mapDarwinPathError(err)
			}
			created = false
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return mapDarwinPathError(openErr)
		}
		if created {
			if err := unix.Fchmod(next, uint32(perm)); err != nil {
				_ = unix.Close(next)
				return err
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return nil
}

func rootWriteFileAtomic(root, rel string, data []byte, perm os.FileMode) error {
	parentFD, base, err := openDarwinParent(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".stable-tmp-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(parentFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return mapDarwinPathError(err)
	}
	keep := false
	defer func() {
		_ = unix.Close(fd)
		if !keep {
			_ = unix.Unlinkat(parentFD, tmp, 0)
		}
	}()
	if err := unix.Fchmod(fd, uint32(perm)); err != nil {
		return err
	}
	for len(data) > 0 {
		n, writeErr := unix.Write(fd, data)
		if writeErr != nil {
			return writeErr
		}
		if n == 0 {
			return errors.New("secfile: short write")
		}
		data = data[n:]
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := ensureDarwinRegularTarget(parentFD, base); err != nil {
		return err
	}
	if err := unix.Renameat(parentFD, tmp, parentFD, base); err != nil {
		return mapDarwinPathError(err)
	}
	keep = true
	return unix.Fsync(parentFD)
}

func rootRemoveFile(root, rel string) error {
	parentFD, base, err := openDarwinParent(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	if err := ensureDarwinRegularTarget(parentFD, base); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, base, 0); err != nil {
		return mapDarwinPathError(err)
	}
	return unix.Fsync(parentFD)
}

func rootReadDir(root, rel string) ([]os.DirEntry, error) {
	parts, err := validateDarwinRelative(rel)
	if err != nil {
		return nil, err
	}
	fd, err := openDarwinRootDir(root)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return nil, mapDarwinPathError(openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	file := os.NewFile(uintptr(fd), rel)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("secfile: unable to wrap directory handle")
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func openDarwinRootDir(root string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, mapDarwinPathError(err)
	}
	return fd, nil
}

func openDarwinParent(root, rel string) (int, string, error) {
	parts, err := validateDarwinRelative(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := openDarwinRootDir(root)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, "", mapDarwinPathError(openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func ensureDarwinRegularTarget(parentFD int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return mapDarwinPathError(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafePath
	}
	return nil
}
