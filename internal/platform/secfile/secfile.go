package secfile

import (
	"errors"
	"os"
)

var (
	ErrUnsafePath      = errors.New("unsafe candidate path")
	ErrDifferentDevice = errors.New("paths are on different filesystems")
)

// SecureOpen opens rel under root with openat2 RESOLVE_BENEATH|NO_SYMLINKS
// and O_NOFOLLOW, then rejects non-regular files.
func SecureOpen(root, rel string) (*os.File, error) {
	return secureOpen(root, rel)
}

// OpenNoFollow opens path read-only without following the final symlink.
func OpenNoFollow(path string) (*os.File, error) {
	return openNoFollow(path)
}

// Exchange atomically swaps two directories on the same Linux filesystem.
func Exchange(dirA, dirB string) error {
	return exchange(dirA, dirB)
}

// SameDevice reports an error when pathA and pathB are on different devices.
func SameDevice(pathA, pathB string) error {
	return sameDevice(pathA, pathB)
}
