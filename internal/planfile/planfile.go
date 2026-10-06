// Package planfile manages per-session plan files under .stable/plans.
// A plan file belongs to exactly one session, is created empty with private
// permissions, and its path can never leave the plans directory.
package planfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"stable/internal/platform/secfile"
	"strings"
)

// DirName is the project-relative directory holding per-session plan files.
const DirName = ".stable/plans"

// PlanPath returns the plan file path of one session inside the project.
// A session id that is empty or contains path separators or ".." is rejected
// so the returned path always stays inside the plans directory.
func PlanPath(projectRoot, sessionID string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", errors.New("project root is required")
	}
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	dir := filepath.Join(projectRoot, DirName)
	path := filepath.Join(dir, sessionID+".md")
	if filepath.Dir(path) != dir {
		return "", errors.New("plan path escapes plans directory")
	}
	return path, nil
}

// Ensure returns the plan file path of one session and reports whether the
// file already existed. The plans directory is created with private
// permissions when missing, and the file is created empty with private
// permissions only when it does not exist yet; an existing file is never
// truncated, only tightened to private permissions.
func Ensure(projectRoot, sessionID string) (string, bool, error) {
	path, err := PlanPath(projectRoot, sessionID)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(path)
	stable := filepath.Dir(dir)
	if st, e := os.Lstat(stable); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("state directory must not be a symlink")
	}
	if st, e := os.Lstat(dir); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("plans directory must not be a symlink")
	}
	if err = secfile.MkdirAllPrivate(dir, 0700); err != nil {
		return "", false, err
	}
	if err = secfile.ChmodPrivate(stable, 0700); err != nil {
		return "", false, err
	}
	if err = secfile.ChmodPrivate(dir, 0700); err != nil {
		return "", false, err
	}
	f, err := secfile.OpenFilePrivate(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err = secfile.ChmodPrivate(f.Name(), 0600); err != nil {
			_ = f.Close()
			return "", false, err
		}
		if err = f.Close(); err != nil {
			return "", false, err
		}
		return path, false, nil
	}
	if !os.IsExist(err) {
		return "", false, err
	}
	if err = secureExisting(path); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// Exists reports whether the plan file of one session is present.
func Exists(projectRoot, sessionID string) (bool, error) {
	path, err := PlanPath(projectRoot, sessionID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// validateSessionID rejects ids that could move the plan path outside the
// plans directory.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if strings.ContainsAny(sessionID, `/\`) {
		return errors.New("session id must not contain path separators")
	}
	if strings.Contains(sessionID, "..") {
		return errors.New(`session id must not contain ".."`)
	}
	return nil
}

func secureExisting(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("plan file must not be a symlink")
	}
	if !st.Mode().IsRegular() {
		return errors.New("plan path must be a regular file")
	}
	if st.Mode().Perm()&0077 != 0 {
		if err = secfile.ChmodPrivate(path, 0600); err != nil {
			return fmt.Errorf("secure plan file permissions: %w", err)
		}
	}
	return nil
}
