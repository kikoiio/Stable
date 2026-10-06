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

// MkdirAllPrivate creates path and every missing parent with the given mode,
// then ensures the mode is actually applied (POSIX chmod after mkdir to beat
// umask; Windows current-user ACL for private modes).
func MkdirAllPrivate(path string, perm os.FileMode) error {
	return mkdirAllPrivate(path, perm)
}

// OpenFilePrivate is like os.OpenFile, but newly created files receive a real
// private mode (chmod after create on POSIX; ACL on Windows).
func OpenFilePrivate(path string, flag int, perm os.FileMode) (*os.File, error) {
	return openFilePrivate(path, flag, perm)
}

// ChmodPrivate sets path mode. On POSIX it is os.Chmod; on Windows private
// modes (0700/0600) become a current-user ACL and 0444 becomes read-only.
func ChmodPrivate(path string, perm os.FileMode) error {
	return chmodPrivate(path, perm)
}

// OwnedByCurrentUser reports whether info's owner is the process user.
func OwnedByCurrentUser(info os.FileInfo) (bool, error) {
	return ownedByCurrentUser(info)
}

// IsPrivate reports whether info is private to the current user
// (POSIX: mode & 0077 == 0 and owned by self; Windows: owner match + DACL).
// Query failures are fail-closed errors.
func IsPrivate(info os.FileInfo) (bool, error) {
	return isPrivate(info)
}

// IsPrivatePath performs the same check while retaining the full path needed
// by Windows security APIs.
func IsPrivatePath(path string) (bool, error) { return isPrivatePath(path) }

// FixHint returns a platform-specific remediation string for private-file failures.
func FixHint(kind string) string {
	return fixHint(kind)
}
