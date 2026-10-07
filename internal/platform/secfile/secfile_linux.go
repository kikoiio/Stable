//go:build linux

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
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTDIR) {
		return ErrUnsafePath
	}
	return err
}

func rootMkdirAll(root, rel string, perm os.FileMode) error {
	parts, err := validateLinuxRelativeParts(rel)
	if err != nil {
		return err
	}
	fd, err := openLinuxRootDir(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts {
		created := true
		if err := unix.Mkdirat(fd, part, uint32(perm)); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return mapLinuxPathError(err)
			}
			created = false
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return mapLinuxPathError(openErr)
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
	parentFD, base, err := openLinuxParent(root, rel)
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
		return mapLinuxPathError(err)
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
	if err := ensureLinuxRegularTarget(parentFD, base); err != nil {
		return err
	}
	if err := unix.Renameat(parentFD, tmp, parentFD, base); err != nil {
		return mapLinuxPathError(err)
	}
	keep = true
	return unix.Fsync(parentFD)
}

func rootRemoveFile(root, rel string) error {
	parentFD, base, err := openLinuxParent(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	if err := ensureLinuxRegularTarget(parentFD, base); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, base, 0); err != nil {
		return mapLinuxPathError(err)
	}
	return unix.Fsync(parentFD)
}

func openLinuxRootDir(root string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, mapLinuxPathError(err)
	}
	return fd, nil
}

func validateLinuxRelativeParts(rel string) ([]string, error) {
	if err := validateLinuxRelative(rel); err != nil {
		return nil, err
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

func openLinuxParent(root, rel string) (int, string, error) {
	parts, err := validateLinuxRelativeParts(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := openLinuxRootDir(root)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, "", mapLinuxPathError(openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func ensureLinuxRegularTarget(parentFD int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return mapLinuxPathError(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafePath
	}
	return nil
}
