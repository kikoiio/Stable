package sessionlog

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Prepare(root string) (string, error) {
	dir, err := SessionDir(root)
	if err != nil {
		return "", err
	}
	for _, p := range []string{filepath.Dir(dir), dir} {
		if st, e := os.Lstat(p); e == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("session directory path must not contain symlinks")
		}
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
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".stable/sessions/" {
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
	_, err = f.WriteString(".stable/sessions/\n")
	return err
}
