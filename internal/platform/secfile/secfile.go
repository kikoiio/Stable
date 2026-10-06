package secfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrUnsafePath      = errors.New("unsafe candidate path")
	ErrDifferentDevice = errors.New("paths are on different filesystems")
	ErrRootChanged     = errors.New("secure root changed during operation")
)

// Root is a validated project root. Its identity is captured when it is
// opened and can be checked again before a multi-file operation commits.
// Platform-specific secureOpen implementations enforce the per-entry rules.
type Root struct {
	path     string
	identity string
}

// OpenRoot validates a directory without following a symlink at the root and
// records a platform-provided file identity snapshot.
func OpenRoot(path string) (Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Root{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Root{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Root{}, ErrUnsafePath
	}
	identity, err := rootIdentity(abs)
	if err != nil {
		return Root{}, err
	}
	return Root{path: filepath.Clean(abs), identity: identity}, nil
}

// Path returns the validated absolute root path.
func (r Root) Path() string { return r.path }

// Open opens a regular file relative to the validated root.
func (r Root) Open(rel string) (*os.File, error) {
	if r.path == "" {
		return nil, ErrUnsafePath
	}
	return SecureOpen(r.path, rel)
}

// Stat opens and stats a regular file relative to the validated root.
func (r Root) Stat(rel string) (os.FileInfo, error) {
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// Revalidate confirms that the root still refers to the same directory.
func (r Root) Revalidate() error {
	if r.path == "" || r.identity == "" {
		return ErrUnsafePath
	}
	current, err := rootIdentity(r.path)
	if err != nil {
		return err
	}
	if current != r.identity {
		return ErrRootChanged
	}
	return nil
}

// Close is present so callers can use Root with a future native directory
// handle implementation. The current identity wrapper owns no descriptor.
func (r Root) Close() error { return nil }

func rootIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ErrUnsafePath
	}
	// FileInfo.Sys carries the native inode/file-index data on the supported
	// targets. Including the cleaned path prevents an unrelated directory with
	// a reused native identity from being accepted as this root.
	return fmt.Sprintf("%s|%#v", filepath.Clean(path), info.Sys()), nil
}

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
