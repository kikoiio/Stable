package inputhistory

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"stable/internal/sessionlog"
)

const historyFileName = "input-history.jsonl"

// Prepare creates the project .stable directory with private permissions and
// keeps the history file out of git, mirroring sessionlog.Prepare.
func Prepare(root string) (string, error) {
	root, err := sessionlog.ProjectRoot(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, ".stable")
	if st, e := os.Lstat(dir); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("state directory must not be a symlink")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	if err = ignoreInGit(root); err != nil {
		return "", err
	}
	return dir, nil
}

func historyPath(root string) (string, error) {
	dir, err := Prepare(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, historyFileName)
	if st, e := os.Lstat(path); e == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("input history file must not be a symlink")
		}
		if !st.Mode().IsRegular() {
			return "", errors.New("input history path must be a regular file")
		}
		if st.Mode().Perm()&0077 != 0 {
			if err = os.Chmod(path, 0600); err != nil {
				return "", fmt.Errorf("secure input history permissions: %w", err)
			}
		}
	}
	return path, nil
}

func ignoreInGit(root string) error {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude")
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	p := strings.TrimSpace(string(b))
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	rule := ".stable/" + historyFileName
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == rule {
			return nil
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		if _, err = f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(rule + "\n")
	return err
}
