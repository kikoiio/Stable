//go:build linux || darwin

package workspace

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func safeReadFlags() int { return syscall.O_NOFOLLOW | syscall.O_NONBLOCK }

// openBeneath rejects links on every component of an already opened root.
// Internal symlinks must not expose protected metadata through raced paths.
func openBeneath(root *os.Root, path string, directory bool) (*os.File, error) {
	if path == "." && directory {
		return root.Open(".")
	}
	clean, err := CleanRelative(path)
	if err != nil || clean != path {
		return nil, ErrUnsafePath
	}
	base, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer base.Close()
	currentFD := int(base.Fd())
	var current *os.File
	defer func() {
		if current != nil {
			current.Close()
		}
	}()
	parts := strings.Split(path, "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		if i != len(parts)-1 || directory {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(currentFD, part, flags, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), path)
		if i == len(parts)-1 {
			return file, nil
		}
		if current != nil {
			current.Close()
		}
		current, currentFD = file, fd
	}
	return nil, ErrUnsafePath
}

func validateHardlinks(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrUnavailable
	}
	if stat.Nlink != 1 {
		return ErrUnsafePath
	}
	return nil
}

func rootIdentity(info os.FileInfo) (RootIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return RootIdentity{}, ErrUnsafePath
	}
	return RootIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func allocatedBytes(info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 || stat.Blocks > (int64(^uint64(0)>>1))/512 || info.Size() < 0 {
		return 0, ErrUnavailable
	}
	return max(stat.Blocks*512, info.Size()), nil
}

func allocationUnit(root *os.Root) (int64, error) {
	file, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || stat.Bsize > 1<<20 {
		return 0, ErrUnavailable
	}
	return int64(stat.Bsize), nil
}
