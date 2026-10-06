package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"stable/internal/platform/secfile"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidateID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id")
	}
	return nil
}

func ProjectRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("project root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("project root is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func SessionDir(root string) (string, error) {
	root, err := ProjectRoot(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".stable", "sessions"), nil
}

func SessionPath(root, id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	dir, err := SessionDir(root)
	if err != nil {
		return "", err
	}
	stable := filepath.Dir(dir)
	if st, e := os.Lstat(stable); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("session directory parent must not be a symlink")
	}
	if st, e := os.Lstat(dir); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("session directory must not be a symlink")
	}
	path := filepath.Join(dir, id+".jsonl")
	if st, e := os.Lstat(path); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("session file must not be a symlink")
	} else if e == nil {
		if !st.Mode().IsRegular() {
			return "", errors.New("session path must be a regular file")
		}
		if st.Mode().Perm()&0077 != 0 {
			if err = secfile.ChmodPrivate(path, 0600); err != nil {
				return "", fmt.Errorf("secure session file permissions: %w", err)
			}
		}
	}
	if filepath.Dir(path) != dir {
		return "", errors.New("session path escapes session directory")
	}
	return path, nil
}
